package postgres

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"horopter.dev/horopter/internal/core"
)

// ChangeKey is a change's (start_at, ref) position; ref makes it a total order in a tenant.
type ChangeKey struct {
	Start time.Time
	Ref   string
}

// ChangeQuery selects changes overlapping a window, or by ref; only NewChangeQuery sets tenant.
type ChangeQuery struct {
	tenant string
	From   time.Time
	To     time.Time
	// Subjects matches any subject in a change's scope against any pattern; nil is no filter.
	Subjects core.SubjectPatterns
	Type     string
	Status   string
	Locus    string
	// Source matches when it is any member of a change's Sources.
	Source string
	// Unexported so NewChangeQuery's allowlist check cannot be bypassed; empty values are no filter.
	attributes map[string]string
	// Ref selects one change by primary key; From and To are then ignored.
	Ref   string
	Limit int
	// After, when set, selects only rows strictly after it in (start_at, ref) order.
	After *ChangeKey
	// MaxRowTextBytes cuts to the leading rows whose summed row text fits; zero means no cut.
	MaxRowTextBytes int64
}

// ErrUnscopedQuery reports a query built from the zero TenantID, which would select nothing.
var ErrUnscopedQuery = errors.New(
	"query is not scoped to a tenant; a tenant comes from the authenticated principal")

// FilterableAttributeKeys are the filterable attribute keys; each needs an expression index.
var FilterableAttributeKeys = map[string]struct{}{
	"deployment.environment.name": {},
}

// ErrUnfilterableAttribute reports a key outside FilterableAttributeKeys, wrapped with both.
var ErrUnfilterableAttribute = errors.New("attribute key is not filterable")

// filterableKeys renders the allowlist for an error message, sorted so the text is stable.
func filterableKeys() string {
	return strings.Join(slices.Sorted(maps.Keys(FilterableAttributeKeys)), ", ")
}

// NewChangeQuery is the only ChangeQuery constructor; attrs is validated here.
func NewChangeQuery(
	tenant core.TenantID, from, to time.Time, limit int, attrs map[string]string,
) (ChangeQuery, error) {
	if tenant.IsZero() {
		return ChangeQuery{}, ErrUnscopedQuery
	}
	// Sorted so that a query naming several bad keys always reports the same one.
	for _, k := range slices.Sorted(maps.Keys(attrs)) {
		if _, ok := FilterableAttributeKeys[k]; !ok {
			return ChangeQuery{}, fmt.Errorf("%w: %q; filterable keys are %s",
				ErrUnfilterableAttribute, k, filterableKeys())
		}
	}
	return ChangeQuery{
		tenant: tenant.String(), From: from, To: to, Limit: limit, attributes: attrs,
	}, nil
}

// EventQuery selects raw events; only NewEventQuery sets tenant.
type EventQuery struct {
	tenant string
	Ref    string
	Limit  int
	// AfterID, when set, selects only events whose id sorts after it.
	AfterID string
	// MaxRowTextBytes cuts to the leading rows whose summed row text fits; zero means no cut.
	MaxRowTextBytes int64
}

// NewEventQuery is the only EventQuery constructor; Ref and MaxRowTextBytes are set afterward.
func NewEventQuery(tenant core.TenantID, limit int) (EventQuery, error) {
	if tenant.IsZero() {
		return EventQuery{}, ErrUnscopedQuery
	}
	return EventQuery{tenant: tenant.String(), Limit: limit}, nil
}

// ReadEstimate is a read's row count, total row text and first row's text, measured beforehand.
type ReadEstimate struct {
	Rows          int
	TotalBytes    int64
	FirstRowBytes int64
}

// Fetching one row beyond the limit is what makes truncation detectable.
func overFetch(limit int) (int, bool) {
	if limit <= 0 {
		return 0, false
	}
	return limit + 1, true
}

func trimToLimit[T any](rows []T, limit int) ([]T, bool) {
	if limit <= 0 || len(rows) <= limit {
		return rows, false
	}
	return rows[:limit], true
}

// A read, its estimate and its byte cut share one selection, so the estimate fits the read.
type selection struct {
	sql   string
	args  []any
	order string
	// Only columns a read returns count against the budget.
	columns string
}

// changeColumns are the columns scanChanges reads, in its order.
const changeColumns = `tenant, ref, subject, type, title, actor, start_at, started_at,
	first_seen_at, end_at, status, attributes, last_event_id, sources`

// eventColumns are the columns scanEvents reads, in its order.
const eventColumns = `id, tenant, idempotency_key, schema_version, source, type, subject,
	actor, title, occurred_at, received_at, correlation_ref, attributes, origin, edited,
	locus, lifecycle`

// By ref alone or by window, never both: (tenant, ref) already selects at most one row.
func changeSelection(q ChangeQuery) selection {
	sb := strings.Builder{}
	var args []any
	if q.Ref != "" {
		sb.WriteString(`SELECT c.* FROM change c WHERE c.tenant = $1 AND c.ref = $2`)
		args = []any{q.tenant, q.Ref}
	} else {
		sb.WriteString(`
			SELECT c.* FROM change c
			WHERE c.tenant = $1 AND c.start_at <= $2 AND c.end_effective >= $3`)
		args = []any{q.tenant, q.To.UTC(), q.From.UTC()}
	}
	args = appendChangeFilters(&sb, args, q)
	if q.After != nil {
		args = append(args, q.After.Start.UTC(), q.After.Ref)
		fmt.Fprintf(&sb, " AND (c.start_at, c.ref) > ($%d, $%d)", len(args)-1, len(args))
	}
	sb.WriteString(" ORDER BY c.start_at ASC, c.ref ASC")
	if limit, bounded := overFetch(q.Limit); bounded {
		args = append(args, limit)
		fmt.Fprintf(&sb, " LIMIT $%d", len(args))
	}
	return selection{
		sql: sb.String(), args: args, order: "start_at ASC, ref ASC", columns: changeColumns,
	}
}

// The ref is split in Go so e.id stays a primary-key comparison.
func eventSelection(q EventQuery) selection {
	query := `SELECT e.* FROM event e WHERE e.tenant = $1`
	args := []any{q.tenant}
	if q.Ref != "" {
		parsed, err := core.ParseRef(q.Ref)
		if err != nil {
			// An unparseable ref binds the empty locus, which event_locus_shape makes match nothing.
			args = append(args, q.Ref, "", "")
		} else {
			args = append(args, q.Ref, parsed.Locus().String(), parsed.ProducerRef())
		}
		query += fmt.Sprintf(
			" AND (e.correlation_ref = $%d OR"+
				" (e.correlation_ref = '' AND e.locus = $%d AND e.id = $%d))",
			len(args)-2, len(args)-1, len(args))
	}
	if q.AfterID != "" {
		args = append(args, q.AfterID)
		query += fmt.Sprintf(" AND e.id > $%d", len(args))
	}
	query += " ORDER BY e.id ASC"
	if limit, bounded := overFetch(q.Limit); bounded {
		args = append(args, limit)
		query += fmt.Sprintf(" LIMIT $%d", len(args))
	}
	return selection{sql: query, args: args, order: "id ASC", columns: eventColumns}
}

// The running sum is filtered one level up: WHERE applies before window functions.
func byteCutSQL(sel selection, budgetArg int) string {
	return fmt.Sprintf(`
		SELECT %[1]s, more FROM (
			SELECT l.*,
			       sum(octet_length(ROW(%[1]s)::text))
			         OVER (ORDER BY %[2]s ROWS UNBOUNDED PRECEDING) AS running,
			       lead(true, 1, false) OVER (ORDER BY %[2]s) AS more
			FROM (%[3]s) l
		) w
		WHERE running <= $%[4]d
		ORDER BY %[2]s`, sel.columns, sel.order, sel.sql, budgetArg)
}

// A cut that returns no rows asks whether there were any to discard.
func (s *Store) anyRows(ctx context.Context, sel selection) (bool, error) {
	var exists bool
	err := s.pool.QueryRow(ctx, "SELECT EXISTS ("+sel.sql+")", sel.args...).Scan(&exists)
	if err != nil {
		return false, fmt.Errorf("check whether a cut read had rows: %w", err)
	}
	return exists, nil
}

// ListEvents lists a tenant's events by id, optionally for one projection ref, with truncation.
func (s *Store) ListEvents(
	ctx context.Context, q EventQuery,
) (evs []core.Event, truncated bool, err error) {
	ctx, span := tracer().Start(ctx, "ListEvents")
	defer span.End()
	// Deferred, so a failed query still records its tenant and limit.
	defer func() {
		span.SetAttributes(
			attribute.String("horopter.tenant", q.tenant),
			attribute.Int("horopter.query.limit", q.Limit),
			attribute.Int("horopter.query.rows", len(evs)),
			attribute.Bool("horopter.query.truncated", truncated),
		)
		if err != nil {
			span.RecordError(err)
			span.SetStatus(codes.Error, "")
		}
	}()

	sel := eventSelection(q)
	if q.MaxRowTextBytes > 0 {
		return s.listEventsWithinBytes(ctx, q, sel)
	}
	rows, err := s.pool.Query(ctx,
		"SELECT "+eventColumns+" FROM ("+sel.sql+") l ORDER BY "+sel.order, sel.args...)
	if err != nil {
		return nil, false, fmt.Errorf("list events for tenant %q: %w", q.tenant, err)
	}
	defer rows.Close()

	evs, err = scanEvents(rows, q.tenant)
	if err != nil {
		return nil, false, err
	}
	evs, truncated = trimToLimit(evs, q.Limit)
	return evs, truncated, nil
}

// listEventsWithinBytes is ListEvents with q.MaxRowTextBytes applied.
func (s *Store) listEventsWithinBytes(
	ctx context.Context, q EventQuery, sel selection,
) ([]core.Event, bool, error) {
	args := append(slices.Clip(sel.args), q.MaxRowTextBytes)
	rows, err := s.pool.Query(ctx, byteCutSQL(sel, len(args)), args...)
	if err != nil {
		return nil, false, fmt.Errorf("list events within %d bytes for tenant %q: %w",
			q.MaxRowTextBytes, q.tenant, err)
	}
	defer rows.Close()

	var more bool
	evs, err := scanEvents(rows, q.tenant, &more)
	if err != nil {
		return nil, false, err
	}
	if len(evs) == 0 {
		cut, err := s.anyRows(ctx, sel)
		return nil, cut, err
	}
	evs, truncated := trimToLimit(evs, q.Limit)
	return evs, truncated || more, nil
}

func scanEvents(rows pgx.Rows, tenantID string, extra ...any) ([]core.Event, error) {
	var evs []core.Event
	for rows.Next() {
		var (
			ev     core.Event
			idem   *string
			origin string
			corr   string
			locus  string
		)
		dest := []any{&ev.ID, &ev.Tenant, &idem, &ev.SchemaVersion, &ev.Source, &ev.Type,
			&ev.Subject, &ev.Actor, &ev.Title, &ev.OccurredAt, &ev.ReceivedAt,
			&corr, &ev.Attributes, &origin, &ev.Edited, &locus, &ev.Lifecycle}
		if err := rows.Scan(append(dest, extra...)...); err != nil {
			return nil, fmt.Errorf("scan event row for tenant %q: %w", tenantID, err)
		}
		if idem != nil {
			ev.IdempotencyKey = *idem
		}
		ev.Origin = core.Origin(origin)
		ev.CorrelationRef = core.RefFromRow(corr)
		ev.Locus = core.LocusFromRow(locus)
		evs = append(evs, ev)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate events for tenant %q: %w", tenantID, err)
	}
	return evs, nil
}

// QueryChanges returns changes overlapping [q.From, q.To] by start_at, reporting truncation.
func (s *Store) QueryChanges(
	ctx context.Context, q ChangeQuery,
) (out []core.Change, truncated bool, err error) {
	ctx, span := tracer().Start(ctx, "QueryChanges")
	defer span.End()
	// Deferred, so a failed query still records its tenant and window.
	defer func() {
		span.SetAttributes(
			attribute.String("horopter.tenant", q.tenant),
			attribute.Int("horopter.query.limit", q.Limit),
			attribute.Int("horopter.query.rows", len(out)),
			attribute.Bool("horopter.query.truncated", truncated),
			attribute.Int64("horopter.query.window_ms", q.To.Sub(q.From).Milliseconds()),
		)
		if err != nil {
			span.RecordError(err)
			span.SetStatus(codes.Error, "")
		}
	}()

	sel := changeSelection(q)
	if q.MaxRowTextBytes > 0 {
		return s.queryChangesWithinBytes(ctx, q, sel)
	}
	rows, err := s.pool.Query(ctx,
		"SELECT "+changeColumns+" FROM ("+sel.sql+") l ORDER BY "+sel.order, sel.args...)
	if err != nil {
		return nil, false, fmt.Errorf("query changes for tenant %q: %w", q.tenant, err)
	}
	defer rows.Close()

	out, err = scanChanges(rows)
	if err != nil {
		return nil, false, err
	}
	out, truncated = trimToLimit(out, q.Limit)
	return out, truncated, nil
}

// queryChangesWithinBytes is QueryChanges with q.MaxRowTextBytes applied.
func (s *Store) queryChangesWithinBytes(
	ctx context.Context, q ChangeQuery, sel selection,
) ([]core.Change, bool, error) {
	args := append(slices.Clip(sel.args), q.MaxRowTextBytes)
	rows, err := s.pool.Query(ctx, byteCutSQL(sel, len(args)), args...)
	if err != nil {
		return nil, false, fmt.Errorf("query changes within %d bytes for tenant %q: %w",
			q.MaxRowTextBytes, q.tenant, err)
	}
	defer rows.Close()

	var more bool
	out, err := scanChanges(rows, &more)
	if err != nil {
		return nil, false, err
	}
	if len(out) == 0 {
		cut, err := s.anyRows(ctx, sel)
		return nil, cut, err
	}
	out, truncated := trimToLimit(out, q.Limit)
	return out, truncated || more, nil
}

// Attribute keys are SQL literals from the allowlist: a bound $n would skip the expression index.
func appendChangeFilters(sb *strings.Builder, args []any, q ChangeQuery) []any {
	if !q.Subjects.IsZero() {
		args = append(args, q.Subjects.Regexp())
		fmt.Fprintf(sb, ` AND EXISTS (SELECT 1 FROM jsonb_each_text(subject_first_seen) s
			WHERE s.key ~ $%d AND (end_at IS NULL
			OR s.value::bigint <= (extract(epoch FROM end_at) * 1000000)::bigint))`, len(args))
	}
	if q.Type != "" {
		args = append(args, q.Type)
		fmt.Fprintf(sb, " AND type = $%d", len(args))
	}
	if q.Status != "" {
		args = append(args, q.Status)
		fmt.Fprintf(sb, " AND status = $%d", len(args))
	}
	if q.Locus != "" {
		args = append(args, q.Locus)
		fmt.Fprintf(sb, " AND locus = $%d", len(args))
	}
	if q.Source != "" {
		args = append(args, q.Source)
		fmt.Fprintf(sb, " AND sources @> ARRAY[$%d]", len(args))
	}
	for _, k := range slices.Sorted(maps.Keys(FilterableAttributeKeys)) {
		v := q.attributes[k]
		if v == "" {
			continue
		}
		args = append(args, v)
		fmt.Fprintf(sb, " AND attributes ->> '%s' = $%d", k, len(args))
	}
	return args
}

// start_at is read as stored, not recomputed from the two facts.
func scanChanges(rows pgx.Rows, extra ...any) ([]core.Change, error) {
	var out []core.Change
	for rows.Next() {
		var (
			c         core.Change
			startedAt *time.Time
			endAt     *time.Time
			st        string
			ref       string
		)
		dest := []any{&c.Tenant, &ref, &c.Subject, &c.Type, &c.Title,
			&c.Actor, &c.Start, &startedAt, &c.FirstSeenAt, &endAt, &st,
			&c.Attributes, &c.LastEventID, &c.Sources}
		if err := rows.Scan(append(dest, extra...)...); err != nil {
			return nil, fmt.Errorf("scan change row: %w", err)
		}
		c.Ref = core.RefFromRow(ref)
		c.StartedAt = startedAt
		c.End = endAt
		c.Status = core.Status(st)
		out = append(out, c)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate change rows: %w", err)
	}
	return out, nil
}

// TenantExists reports whether a tenant row named id exists, for egress's global credentials.
func (s *Store) TenantExists(ctx context.Context, id string) (bool, error) {
	var exists bool
	err := s.pool.QueryRow(ctx,
		"SELECT EXISTS (SELECT 1 FROM tenant WHERE id = $1)", id).Scan(&exists)
	if err != nil {
		return false, fmt.Errorf("check whether tenant %q exists: %w", id, err)
	}
	return exists, nil
}

// EstimateChanges measures what QueryChanges would fetch for q; q.MaxRowTextBytes is ignored.
func (s *Store) EstimateChanges(ctx context.Context, q ChangeQuery) (ReadEstimate, error) {
	return s.estimate(ctx, "EstimateChanges", q.tenant, changeSelection(q))
}

// EstimateEvents measures the rows ListEvents would fetch for q, as EstimateChanges does.
func (s *Store) EstimateEvents(ctx context.Context, q EventQuery) (ReadEstimate, error) {
	return s.estimate(ctx, "EstimateEvents", q.tenant, eventSelection(q))
}

func (s *Store) estimate(
	ctx context.Context, operation, tenantID string, sel selection,
) (est ReadEstimate, err error) {
	ctx, span := tracer().Start(ctx, operation)
	defer span.End()
	defer func() {
		span.SetAttributes(attribute.String("horopter.tenant", tenantID))
		if err != nil {
			span.RecordError(err)
			span.SetStatus(codes.Error, "")
		}
	}()

	query := fmt.Sprintf(`
		SELECT count(*), coalesce(sum(b), 0), coalesce((array_agg(b ORDER BY %s))[1], 0)
		FROM (SELECT octet_length(ROW(%s)::text) AS b, l.* FROM (%s) l) s`,
		sel.order, sel.columns, sel.sql)
	err = s.pool.QueryRow(ctx, query, sel.args...).Scan(
		&est.Rows, &est.TotalBytes, &est.FirstRowBytes)
	if err != nil {
		return ReadEstimate{}, fmt.Errorf("estimate a read for tenant %q: %w", tenantID, err)
	}
	return est, nil
}
