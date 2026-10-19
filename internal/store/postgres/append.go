package postgres

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/trace"
	"horopter.dev/horopter/internal/core"
	"horopter.dev/horopter/internal/telemetry"
)

const uniqueViolation = "23505"

// Matched so a 503's log line tells a running rebuild from a down database.
const lockNotAvailable = "55P03"

// AppendLockTimeout bounds an append's lock wait: far above contention, inside HTTP timeouts.
const AppendLockTimeout = 3 * time.Second

// Matched by name, so a duplicate event id is not mistaken for a retry.
const idemConstraint = "uq_event_idem"

// appendCounter creates horopter.event.appends, which counts events, not changes.
func appendCounter() metric.Int64Counter {
	counter, err := otel.Meter(telemetry.StoreScope).Int64Counter("horopter.event.appends",
		metric.WithUnit("{append}"),
		metric.WithDescription("Events submitted to the store, by outcome"))
	if err != nil {
		// The SDK returns a working instrument with the error, so the store opens either way.
		slog.Error("create the horopter.event.appends counter", "error", err)
	}
	return counter
}

// AppendEvent stores and folds an event in one transaction; a duplicate key returns the original.
func (s *Store) AppendEvent(
	ctx context.Context, ev core.Event,
) (stored core.Event, created bool, err error) {
	ctx, span := tracer().Start(ctx, "AppendEvent")
	defer span.End()

	// Not prepareEvent's result, which is zero on the very failures these attributes debug.
	submitted := ev
	out := appendOutcome{dedupePath: dedupeNone}
	defer func() {
		out.created = created
		out.errKind = errKindOf(err)
		s.recordAppendOutcome(ctx, span, out, submitted, err)
	}()

	ev, err = prepareEvent(ev, s.maxAttributeBytes)
	if err != nil {
		return core.Event{}, false, err
	}

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return core.Event{}, false, fmt.Errorf("begin transaction: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	if err = setLockTimeout(ctx, tx, AppendLockTimeout, "append"); err != nil {
		return core.Event{}, false, err
	}

	existing, found, err := findExistingDuplicate(ctx, tx, ev)
	if err != nil {
		return core.Event{}, false, err
	}
	if found {
		out.dedupePath = dedupeLookup
		return existing, false, tx.Commit(ctx)
	}
	return s.writeEvent(ctx, tx, ev, &out)
}

func (s *Store) recordAppendOutcome(
	ctx context.Context, span trace.Span, out appendOutcome, submitted core.Event, err error,
) {
	span.SetAttributes(out.attributes(submitted)...)
	s.appends.Add(ctx, 1, metric.WithAttributeSet(out.metricAttributes()))
	// Only on success: a re-fold that errored would otherwise count as nothing moved.
	if out.outOfOrder && err == nil {
		// Boolean, not the field list: eight field names would give 256 series.
		s.refolds.Add(ctx, 1, metric.WithAttributeSet(attribute.NewSet(
			attribute.Bool("horopter.refold.altered",
				movedSomethingRecorded(out.refoldMoved)))))
	}
	if err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, out.errKind)
	}
}

// A nil map becomes empty: DEFAULT never fires for an explicit NULL.
func prepareEvent(ev core.Event, maxAttributeBytes int) (core.Event, error) {
	if err := ev.Validate(); err != nil {
		return core.Event{}, err
	}
	if maxAttributeBytes > 0 {
		if err := ev.CheckAttributeSize(maxAttributeBytes); err != nil {
			return core.Event{}, err
		}
	}
	if ev.Attributes == nil {
		ev.Attributes = map[string]any{}
	}
	return ev, nil
}

func findExistingDuplicate(
	ctx context.Context, tx pgx.Tx, ev core.Event,
) (core.Event, bool, error) {
	if ev.IdempotencyKey == "" {
		return core.Event{}, false, nil
	}
	existing, found, err := findByIdemKey(ctx, tx, ev.Tenant, ev.IdempotencyKey)
	if err != nil {
		return core.Event{}, false, err
	}
	if found {
		return existing, true, nil
	}
	if afterIdemLookupHook != nil {
		afterIdemLookupHook()
	}
	return core.Event{}, false, nil
}

// The 23505 arm is the concurrent-retry path; see recoverConcurrentDuplicate.
func (s *Store) writeEvent(
	ctx context.Context, tx pgx.Tx, ev core.Event, out *appendOutcome,
) (core.Event, bool, error) {
	if err := insertEvent(ctx, tx, ev); err != nil {
		if isUniqueViolation(err, idemConstraint) {
			return s.recoverConcurrentDuplicate(ctx, tx, ev, out)
		}
		return core.Event{}, false, err
	}
	if err := applyToProjection(ctx, tx, ev, out); err != nil {
		return core.Event{}, false, err
	}
	if err := tx.Commit(ctx); err != nil {
		return core.Event{}, false, fmt.Errorf("commit event %q: %w", ev.ID, err)
	}
	return ev, true, nil
}

// An empty key binds as NULL: uq_event_idem is partial on IS NOT NULL.
func insertEvent(ctx context.Context, tx pgx.Tx, ev core.Event) error {
	var idem *string
	if ev.IdempotencyKey != "" {
		idem = &ev.IdempotencyKey
	}
	if _, err := tx.Exec(ctx, `
		INSERT INTO event (id, tenant, idempotency_key, schema_version, source, type,
		                   subject, actor, title, occurred_at, received_at,
		                   correlation_ref, attributes, origin, edited, locus, lifecycle)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,$17)`,
		ev.ID, ev.Tenant, idem, ev.SchemaVersion, ev.Source, ev.Type,
		ev.Subject, ev.Actor, ev.Title, ev.OccurredAt.UTC(), ev.ReceivedAt.UTC(),
		ev.CorrelationRef.String(), ev.Attributes, string(ev.Origin), ev.Edited,
		ev.Locus.String(), string(ev.Lifecycle),
	); err != nil {
		return fmt.Errorf("insert event %q: %w", ev.ID, err)
	}
	return nil
}

func isUniqueViolation(err error, constraint string) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) &&
		pgErr.Code == uniqueViolation && pgErr.ConstraintName == constraint
}

func isLockTimeout(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == lockNotAvailable
}

// SET LOCAL via set_config, which takes a bind parameter; callers issue it first.
func setLockTimeout(ctx context.Context, tx pgx.Tx, d time.Duration, what string) error {
	if _, err := tx.Exec(ctx, `SELECT set_config('lock_timeout', $1, true)`,
		strconv.FormatInt(d.Milliseconds(), 10)); err != nil {
		return fmt.Errorf("bound the %s lock wait at %s: %w", what, d, err)
	}
	return nil
}

// Looks up on the pool: Postgres aborts tx on any error, and the winner has committed by now.
func (s *Store) recoverConcurrentDuplicate(
	ctx context.Context, tx pgx.Tx, ev core.Event, out *appendOutcome,
) (core.Event, bool, error) {
	_ = tx.Rollback(ctx)
	out.dedupePath = dedupeUniqueViolation

	existing, found, err := findByIdemKey(ctx, s.pool, ev.Tenant, ev.IdempotencyKey)
	if err != nil {
		return core.Event{}, false, err
	}
	if !found {
		return core.Event{}, false, fmt.Errorf(
			"event %q: idempotency key %q/%q collided with a concurrent write but no "+
				"stored event carries it; retry the append",
			ev.ID, ev.Tenant, ev.IdempotencyKey)
	}
	return existing, false, nil
}

// findByIdemKey returns the previously stored event for (tenant, idempotencyKey), if any.
func findByIdemKey(
	ctx context.Context, q querier, tenantID, idempotencyKey string,
) (core.Event, bool, error) {
	var (
		ev     core.Event
		idem   *string
		origin string
		corr   string
		locus  string
	)
	err := q.QueryRow(ctx, `
		SELECT id, tenant, idempotency_key, schema_version, source, type, subject,
		       actor, title, occurred_at, received_at, correlation_ref, attributes,
		       origin, edited, locus, lifecycle
		FROM event WHERE tenant = $1 AND idempotency_key = $2`,
		tenantID, idempotencyKey,
	).Scan(&ev.ID, &ev.Tenant, &idem, &ev.SchemaVersion, &ev.Source, &ev.Type, &ev.Subject,
		&ev.Actor, &ev.Title, &ev.OccurredAt, &ev.ReceivedAt, &corr, &ev.Attributes,
		&origin, &ev.Edited, &locus, &ev.Lifecycle)
	if errors.Is(err, pgx.ErrNoRows) {
		return core.Event{}, false, nil
	}
	if err != nil {
		return core.Event{}, false, fmt.Errorf(
			"find event by idempotency key %q/%q: %w", tenantID, idempotencyKey, err)
	}
	if idem != nil {
		ev.IdempotencyKey = *idem
	}
	ev.Origin = core.Origin(origin)
	ev.CorrelationRef = core.RefFromRow(corr)
	ev.Locus = core.LocusFromRow(locus)
	return ev, true, nil
}

// Nil in production; tests widen the read-to-write window with it.
var afterLoadHook func()

// Nil in production; tests widen the lookup-to-insert window with it.
var afterIdemLookupHook func()

// Nil in production; tests hold a rebuild between its DELETE and re-insert.
var afterDeleteHook func()

func applyToProjection(ctx context.Context, tx pgx.Tx, ev core.Event, out *appendOutcome) error {
	ref := core.RefFor(ev).String()
	if err := lockProjectionWrite(ctx, tx, ev.Tenant, ref, out); err != nil {
		return err
	}
	cur, found, err := lockAndLoadChange(ctx, tx, ev.Tenant, ref)
	if err != nil {
		return err
	}
	if afterLoadHook != nil {
		afterLoadHook()
	}
	var prev *core.Change
	if found {
		prev = &cur
	}
	folded := core.Fold(prev, ev)
	// An id below the watermark is where incremental and id-ordered folds disagree, so re-fold.
	out.outOfOrder = found && ev.ID < cur.LastEventID
	if !out.outOfOrder {
		out.foldStatus = folded.Status
		out.foldClosed = folded.End != nil
		return upsertChange(ctx, tx, folded)
	}
	return refoldOutOfOrderAppend(ctx, tx, ev, folded, out)
}

// folded is passed in so this can report what the re-fold moved.
func refoldOutOfOrderAppend(
	ctx context.Context, tx pgx.Tx, ev core.Event, folded core.Change, out *appendOutcome,
) error {
	refRef := core.RefFor(ev)
	reref, err := refoldRef(ctx, tx, ev.Tenant, refRef)
	if err != nil {
		return err
	}
	// refoldRef returns the change it wrote, so no second FOR UPDATE here.
	refolded := reref.change
	out.foldStatus = refolded.Status
	out.foldClosed = refolded.End != nil
	out.refoldMoved = alteredFields(folded, refolded)
	// Status and End are what the log says happened; movedSomethingRecorded is their one test.
	if movedSomethingRecorded(out.refoldMoved) {
		slog.WarnContext(ctx, "a re-fold moved a recorded outcome",
			"tenant", ev.Tenant, "ref", refRef.String(),
			"was", string(folded.Status), "now", string(refolded.Status),
			"moved", out.refoldMoved)
	}
	return nil
}

// The per-ref exclusive lock serialises the fold; the tenant shared lock excludes rebuilds.
func lockProjectionWrite(
	ctx context.Context, tx pgx.Tx, tenantID, ref string, out *appendOutcome,
) error {
	start := time.Now()
	_, err := tx.Exec(ctx, `
		SELECT pg_advisory_xact_lock_shared(hashtext($1)),
		       pg_advisory_xact_lock(hashtext($1 || '/' || $2))`,
		tenantID, ref)
	out.lockWait = time.Since(start)
	if err != nil {
		if isLockTimeout(err) {
			out.lockTimeout = true
			return fmt.Errorf(
				"gave up after %s waiting for the projection lock on %q/%q: %w; the "+
					"usual cause is a projection rebuild for this tenant, which holds "+
					"the lock for the whole of its run, but any long-held transaction "+
					"on this tenant or ref will do it — nothing was written, so retry "+
					"the append",
				AppendLockTimeout, tenantID, ref, err)
		}
		return fmt.Errorf(
			"take projection lock for %q/%q: %w; nothing was written, so the append "+
				"can be retried", tenantID, ref, err)
	}
	return nil
}

// FOR UPDATE stays as a guard against a writer that skipped the advisory lock.
func lockAndLoadChange(
	ctx context.Context, tx pgx.Tx, tenantID, ref string,
) (core.Change, bool, error) {
	var (
		c         core.Change
		startedAt *time.Time
		endAt     *time.Time
		st        string
		storedRef string
	)
	err := tx.QueryRow(ctx, `
		SELECT tenant, ref, subject, type, title, actor, start_at, started_at,
		       first_seen_at, end_at, status, attributes, last_event_id, sources,
		       subject_first_seen, title_started_at
		FROM change WHERE tenant = $1 AND ref = $2 FOR UPDATE`,
		tenantID, ref,
	).Scan(&c.Tenant, &storedRef, &c.Subject, &c.Type, &c.Title, &c.Actor,
		&c.Start, &startedAt, &c.FirstSeenAt, &endAt, &st, &c.Attributes,
		&c.LastEventID, &c.Sources, &c.SubjectFirstSeen, &c.TitleStartedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return core.Change{}, false, nil
	}
	if err != nil {
		return core.Change{}, false, fmt.Errorf("lock change %q/%q: %w", tenantID, ref, err)
	}
	c.Ref = core.RefFromRow(storedRef)
	c.StartedAt = startedAt
	c.End = endAt
	c.Status = core.Status(st)
	return c, true, nil
}

// Generated columns are omitted: Postgres rejects a supplied value for any of them.
func upsertChange(ctx context.Context, tx pgx.Tx, c core.Change) error {
	_, err := tx.Exec(ctx, `
		INSERT INTO change (tenant, ref, subject, type, title, actor, started_at,
		                    first_seen_at, end_at, status, attributes, last_event_id, sources,
		                    subject_first_seen, title_started_at)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15)
		ON CONFLICT (tenant, ref) DO UPDATE SET
		  subject=EXCLUDED.subject, type=EXCLUDED.type, title=EXCLUDED.title,
		  actor=EXCLUDED.actor, started_at=EXCLUDED.started_at,
		  first_seen_at=EXCLUDED.first_seen_at, end_at=EXCLUDED.end_at,
		  status=EXCLUDED.status, attributes=EXCLUDED.attributes,
		  last_event_id=EXCLUDED.last_event_id, sources=EXCLUDED.sources,
		  subject_first_seen=EXCLUDED.subject_first_seen,
		  title_started_at=EXCLUDED.title_started_at`,
		c.Tenant, c.Ref.String(), c.Subject, c.Type, c.Title, c.Actor,
		c.StartedAt, c.FirstSeenAt, c.End, string(c.Status), c.Attributes, c.LastEventID,
		c.Sources, c.SubjectFirstSeen, c.TitleStartedAt,
	)
	if err != nil {
		return fmt.Errorf("upsert change %q/%q: %w", c.Tenant, c.Ref, err)
	}
	return nil
}
