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

// ChangeQuery selects changes overlapping a time window, or by ref alone. tenant is
// unexported: NewChangeQuery is the only way to set it, so a query naming no tenant is a
// compile error, not a struct literal that happens to select nothing.
type ChangeQuery struct {
	tenant  string
	From    time.Time
	To      time.Time
	Subject string
	Type    string
	Status  string
	Locus   string
	// Source filters for one system among a change's Sources: a change HAS several,
	// a caller FILTERS for one. Matches when Source is any member of the set.
	Source string
	// attributes filters on exact attribute values, by key. Only keys in
	// FilterableAttributeKeys are accepted; NewChangeQuery refuses any other rather than
	// ignoring it, because a filter that silently matches everything answers the caller's
	// question wrongly. An empty value is no filter, as an empty Subject is. It is
	// unexported so NewChangeQuery's allowlist check is the only way to set it — a field a
	// caller could assign after construction would let that check be bypassed entirely.
	attributes map[string]string
	// Ref selects a single change by its (tenant, ref) primary key. When set, the window
	// predicate is omitted entirely: From and To are ignored, since a ref lookup is already
	// exact and a window could only narrow it redundantly.
	Ref   string
	Limit int
	// MaxRowTextBytes cuts the result after the leading rows whose row text, summed in order,
	// fits within it; zero means no cut. Row text is a row's size as Postgres renders the whole
	// row as text, octet_length(row::text). A cut that discards rows reports the result
	// truncated.
	MaxRowTextBytes int64
}

// ErrUnscopedQuery reports a query constructed from the zero core.TenantID.
//
// TenantID's unexported field is the compile-time half of the tenant gate: a query naming no
// tenant cannot be written as a struct literal. Its zero value is still constructible, and a
// query carrying one would select the empty tenant: an empty result set indistinguishable from a
// legitimate one, which is the failure mode that makes this worth a runtime guard rather than
// a convention. Nothing reaches it today; a future route registered without egress's admit
// would.
var ErrUnscopedQuery = errors.New(
	"query is not scoped to a tenant; a tenant comes from the authenticated principal")

// FilterableAttributeKeys are the attribute keys a change query may filter on. Each one has
// an expression index in the migration that added it; adding a key means adding both, and
// the cost is the point. The list is a compatibility contract — an attribute key that
// becomes filterable is one callers may depend on — and attributes are otherwise free-form
// and producer-chosen, so without an allowlist every key a producer invents becomes a query
// surface nobody designed and nobody can withdraw.
var FilterableAttributeKeys = map[string]struct{}{
	"deployment.environment.name": {},
}

// ErrUnfilterableAttribute reports a change query naming an attribute key outside
// FilterableAttributeKeys. It is always wrapped with the offending key and the permitted
// list, so egress can answer 400 with the error's own text.
var ErrUnfilterableAttribute = errors.New("attribute key is not filterable")

// filterableKeys renders the allowlist for an error message, sorted so the text is stable.
func filterableKeys() string {
	return strings.Join(slices.Sorted(maps.Keys(FilterableAttributeKeys)), ", ")
}

// NewChangeQuery returns a ChangeQuery scoped to tenant over [from, to] — the only
// constructor for one, and so the only place a core.TenantID crosses into the store
// package. Subject, Type, Status, Locus, Source and Ref are optional filters, assigned on
// the result afterward.
//
// attrs is a parameter rather than a field set afterward because it is the only filter that
// can be invalid: an unallowlisted key must never reach the store, and a constructor is the
// one place that can be guaranteed. Pass nil for no attribute filter.
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

// EventQuery selects raw events for audit, export, and debugging. tenant is unexported for
// the same reason ChangeQuery's is: NewEventQuery is the only way to set it.
type EventQuery struct {
	tenant string
	Ref    string
	Limit  int
	// MaxRowTextBytes cuts the result after the leading rows whose row text, summed in order,
	// fits within it; zero means no cut. Row text is a row's size as Postgres renders the whole
	// row as text, octet_length(row::text). A cut that discards rows reports the result
	// truncated.
	MaxRowTextBytes int64
}

// NewEventQuery returns an EventQuery scoped to tenant, the only constructor for one. Ref
// and MaxRowTextBytes are optional and assigned on the result afterward.
func NewEventQuery(tenant core.TenantID, limit int) (EventQuery, error) {
	if tenant.IsZero() {
		return EventQuery{}, ErrUnscopedQuery
	}
	return EventQuery{tenant: tenant.String(), Limit: limit}, nil
}

// ReadEstimate is the size of what a read will fetch, measured before it runs: how many rows,
// their total row text, and the first row's row text, in the read's order.
type ReadEstimate struct {
	Rows          int
	TotalBytes    int64
	FirstRowBytes int64
}

// overFetch returns the LIMIT to send for a caller-requested limit, and reports whether a
// limit applies at all. It is the caller's limit plus one: fetching a row beyond what was
// asked for is what makes truncation DETECTABLE, because counting to the limit alone
// cannot tell a result set of exactly Limit rows from one with more behind it. A limit of
// zero or less means "no limit", for which no LIMIT clause is emitted.
func overFetch(limit int) (int, bool) {
	if limit <= 0 {
		return 0, false
	}
	return limit + 1, true
}

// trimToLimit discards the over-fetched row and reports that the result was truncated.
// The extra row never reaches the caller, so ChangeQuery.Limit and EventQuery.Limit still
// mean exactly what the caller asked for.
func trimToLimit[T any](rows []T, limit int) ([]T, bool) {
	if limit <= 0 || len(rows) <= limit {
		return rows, false
	}
	return rows[:limit], true
}

// selection is SQL selecting whole rows of one table for a read — its tenant, filters, order
// and over-fetch limit — with the arguments it takes and the order its columns are sorted by.
// A read's query, its estimate and its byte cut all select from the same selection, which is
// what makes an estimate describe exactly the rows the read then fetches.
type selection struct {
	sql   string
	args  []any
	order string
}

// changeColumns are the columns scanChanges reads, in its order.
const changeColumns = `tenant, ref, subject, type, title, actor, start_at, started_at,
	first_seen_at, end_at, status, attributes, last_event_id, sources`

// eventColumns are the columns scanEvents reads, in its order.
const eventColumns = `id, tenant, idempotency_key, schema_version, source, type, subject,
	actor, title, occurred_at, received_at, correlation_ref, attributes, origin, edited,
	locus, lifecycle`

// changeSelection selects the change rows q reads: by ref alone when q.Ref is set, or by
// window otherwise. The two predicates are mutually exclusive, not layered — a window
// alongside a ref would be redundant, since (tenant, ref) already selects at most one row.
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
	sb.WriteString(" ORDER BY c.start_at ASC, c.ref ASC")
	if limit, bounded := overFetch(q.Limit); bounded {
		args = append(args, limit)
		fmt.Fprintf(&sb, " LIMIT $%d", len(args))
	}
	return selection{sql: sb.String(), args: args, order: "start_at ASC, ref ASC"}
}

// eventSelection selects the event rows q reads. See ListEvents for the ref predicate.
//
// The ref is split in Go rather than in SQL so e.id stays a primary-key comparison; a
// predicate on e.locus || '/' || e.id would compute a value per row and scan the table.
// core.ParseRef is what splits it, so the one statement of the encoding serves the read
// side as well as the write side.
func eventSelection(q EventQuery) selection {
	query := `SELECT e.* FROM event e WHERE e.tenant = $1`
	args := []any{q.tenant}
	if q.Ref != "" {
		parsed, err := core.ParseRef(q.Ref)
		if err != nil {
			// A ref that does not parse names no change, so it must match nothing. Binding
			// the impossible locus rather than returning an error keeps this function total
			// and preserves the route's behaviour: a malformed ref answers 200 with an empty
			// list, which TestListEventsIgnoresARefWithNoLocus and
			// TestListEventsIgnoresARefWithAnEmptyLocus pin. The empty locus is what makes
			// it impossible — event_locus_shape forbids one, so no row can carry it, which
			// is the same reason the old strings.Cut form was safe, now stated rather than
			// implied. The correlation_ref arm still binds q.Ref verbatim, so a stored ref
			// ParseRef refuses stays findable by it.
			args = append(args, q.Ref, "", "")
		} else {
			args = append(args, q.Ref, parsed.Locus().String(), parsed.ProducerRef())
		}
		query += fmt.Sprintf(
			" AND (e.correlation_ref = $%d OR"+
				" (e.correlation_ref = '' AND e.locus = $%d AND e.id = $%d))",
			len(args)-2, len(args)-1, len(args))
	}
	query += " ORDER BY e.id ASC"
	if limit, bounded := overFetch(q.Limit); bounded {
		args = append(args, limit)
		query += fmt.Sprintf(" LIMIT $%d", len(args))
	}
	return selection{sql: query, args: args, order: "id ASC"}
}

// byteCutSQL wraps sel so it returns only the leading rows whose row text, summed in sel's
// order, fits within the budget bound at $budgetArg, selecting columns and then whether another
// row followed each one before the cut. For the last row returned, that says whether the cut
// discarded anything.
//
// The running sum is taken over rows already limited to sel's own over-fetch (the caller's
// limit plus one), so it never scans past them, and it is filtered one level up because a WHERE
// clause is applied before the window functions at its own level.
func byteCutSQL(columns string, sel selection, budgetArg int) string {
	return fmt.Sprintf(`
		SELECT %[1]s, more FROM (
			SELECT l.*,
			       sum(octet_length(l::text))
			         OVER (ORDER BY %[2]s ROWS UNBOUNDED PRECEDING) AS running,
			       lead(true, 1, false) OVER (ORDER BY %[2]s) AS more
			FROM (%[3]s) l
		) w
		WHERE running <= $%[4]d
		ORDER BY %[2]s`, columns, sel.order, sel.sql, budgetArg)
}

// anyRows reports whether sel selects any row. A byte cut that returns no rows cannot tell
// from its rows whether there were any to discard, so it asks.
func (s *Store) anyRows(ctx context.Context, sel selection) (bool, error) {
	var exists bool
	err := s.pool.QueryRow(ctx, "SELECT EXISTS ("+sel.sql+")", sel.args...).Scan(&exists)
	if err != nil {
		return false, fmt.Errorf("check whether a cut read had rows: %w", err)
	}
	return exists, nil
}

// ListEvents selects raw events for a tenant, optionally scoped to one change's ref,
// ordered by id ascending (which is chronological, since ids are ULIDs). It also reports
// whether more rows matched than q.Limit allowed through; the over-fetch that detects that
// is invisible to the caller, so q.Limit still means what it says.
//
// q.Ref is a projection ref, matched the way core.RefFor forms one: a correlated event's
// correlation_ref, or an uncorrelated event's locus and own id. Filtering on
// correlation_ref alone made listing a point change's events always return nothing,
// even though that change is in every query result the egress read side serves. The id
// arm is restricted to rows with no correlation_ref so it can only ever match the point
// that owns the ref, never an unrelated event whose id happens to equal some other
// change's correlation_ref.
func (s *Store) ListEvents(
	ctx context.Context, q EventQuery,
) (evs []core.Event, truncated bool, err error) {
	ctx, span := tracer().Start(ctx, "ListEvents")
	defer span.End()
	// Recorded from a defer, not from the success path, so a query that fails still says
	// which tenant asked for what. The limit is known at entry; rows and truncated read
	// from the named results, which on every error path are the zero values this returns.
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
	rows, err := s.pool.Query(ctx, byteCutSQL(eventColumns, sel, len(args)), args...)
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

// scanEvents reads every event row, naming tenantID in any error so a failure identifies
// the query it came from. Extra destinations are scanned after the row's own columns.
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

// QueryChanges returns changes overlapping [q.From, q.To], and reports whether more rows
// matched than q.Limit allowed through.
//
// The predicate is overlap, not start-containment. Filtering on start alone would hide
// a change that began before the window and is still running — precisely the change
// someone most needs to see when zooming into a spike.
//
// The ordering is start_at ASC unconditionally, so truncation discards the most recent
// rows. That is why the truncated flag matters rather than being a nicety: a caller
// otherwise cannot tell a result set of exactly q.Limit rows from one that dropped the
// changes it most needed. The over-fetch that detects it is invisible to the caller —
// q.Limit still means what it says.
func (s *Store) QueryChanges(
	ctx context.Context, q ChangeQuery,
) (out []core.Change, truncated bool, err error) {
	ctx, span := tracer().Start(ctx, "QueryChanges")
	defer span.End()
	// Recorded from a defer, not from the success path, so a query that fails still says
	// which tenant asked for what over how wide a window — the two dimensions a slow or
	// failing dashboard query is diagnosed by. Limit and window are known at entry; rows
	// and truncated read from the named results, which on every error path are the zero
	// values this returns.
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
	rows, err := s.pool.Query(ctx, byteCutSQL(changeColumns, sel, len(args)), args...)
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

// appendChangeFilters adds the optional equality filters to the query and returns the
// extended argument list. The placeholders are numbered from the argument list's own
// length, so the two must grow together — which is why they are built in one place.
//
// The attribute loop ranges over FilterableAttributeKeys rather than over q.attributes, and
// that is not a stylistic choice. The key is written into the SQL text rather than bound,
// because ix_change_attr_environment indexes the expression
// (attributes ->> 'deployment.environment.name') and Postgres matches an expression index by
// comparing expressions: under a generic plan a bound $n is not folded to a constant, the
// expressions stop matching, and the index is silently skipped — which would leave the
// allowlist buying nothing. Ranging over the allowlist means the only strings that ever reach
// the SQL are the literals declared in this file, whatever a caller supplied. The values stay
// bound parameters. Sorting the allowlist also makes the generated SQL deterministic, which
// Go's map iteration order is not.
func appendChangeFilters(sb *strings.Builder, args []any, q ChangeQuery) []any {
	if q.Subject != "" {
		args = append(args, q.Subject)
		fmt.Fprintf(sb, " AND subject = $%d", len(args))
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

// scanChanges reads every change row. start_at is read as well as the two facts it is
// generated from, so a loaded Change carries the same start the database holds rather than
// one recomputed from them. Extra destinations are scanned after the row's own columns.
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

// TenantExists reports whether a tenant row named id exists. It answers egress's global
// credential path: a superuser may name any tenant, and telling a real one from a typo
// needs a lookup, not an inference from an empty result set.
func (s *Store) TenantExists(ctx context.Context, id string) (bool, error) {
	var exists bool
	err := s.pool.QueryRow(ctx,
		"SELECT EXISTS (SELECT 1 FROM tenant WHERE id = $1)", id).Scan(&exists)
	if err != nil {
		return false, fmt.Errorf("check whether tenant %q exists: %w", id, err)
	}
	return exists, nil
}

// EstimateChanges measures the rows QueryChanges would fetch for q without fetching them: the
// same tenant, window, filters, order and over-fetch limit. q.MaxRowTextBytes is ignored; an
// estimate is what decides whether to set it.
func (s *Store) EstimateChanges(ctx context.Context, q ChangeQuery) (ReadEstimate, error) {
	return s.estimate(ctx, "EstimateChanges", q.tenant, changeSelection(q))
}

// EstimateEvents measures the rows ListEvents would fetch for q, as EstimateChanges does.
func (s *Store) EstimateEvents(ctx context.Context, q EventQuery) (ReadEstimate, error) {
	return s.estimate(ctx, "EstimateEvents", q.tenant, eventSelection(q))
}

// estimate sums the row text of every row sel selects and takes the first row's by sel's
// order, under a span named operation.
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
		FROM (SELECT octet_length(l::text) AS b, l.* FROM (%s) l) s`, sel.order, sel.sql)
	err = s.pool.QueryRow(ctx, query, sel.args...).Scan(
		&est.Rows, &est.TotalBytes, &est.FirstRowBytes)
	if err != nil {
		return ReadEstimate{}, fmt.Errorf("estimate a read for tenant %q: %w", tenantID, err)
	}
	return est, nil
}
