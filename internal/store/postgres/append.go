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

// uniqueViolation is the SQLSTATE Postgres raises for a unique constraint or unique
// index violation. github.com/jackc/pgerrcode names every code, but it is a separate
// module and this store needs exactly one of them.
const uniqueViolation = "23505"

// lockNotAvailable is the SQLSTATE Postgres raises when lock_timeout expires while a
// statement waits for a lock. It is matched so an operator reading a 503's server-side log
// line can tell "a rebuild is running" from "the database is down".
const lockNotAvailable = "55P03"

// AppendLockTimeout bounds how long an append waits for the projection locks.
//
// RebuildProjection holds a tenant's lock exclusively for its whole duration, so without a
// bound every append for that tenant queues behind it, each one pinning a connection from
// a pool shared across tenants: a long rebuild stalls ingress for tenants that have
// nothing to do with it. Failing fast instead turns that into a 503 the producer retries
// once the rebuild finishes — the event is delayed rather than lost, which is strictly the
// better failure.
//
// Three seconds is two orders of magnitude above ordinary same-ref contention (two appends
// to one ref is a few milliseconds of read-modify-write), so a rebuild is the usual cause
// when it does fire — but it is not the only one a bound of this size can reach. Anything
// else holding the tenant or ref lock long enough gets here too: a pathologically slow
// append, a hung transaction, a database under enough load that milliseconds become
// seconds. The value is also well inside a normal HTTP client timeout, so the producer
// receives a status it can act on rather than a client-side timeout carrying none.
const AppendLockTimeout = 3 * time.Second

// idemConstraint is the unique index enforcing one event per (tenant, idempotency_key).
// A 23505 is matched against this name rather than against the error text, so a
// different unique violation — a duplicate event id, say — is still reported as the
// error it is instead of being mistaken for a retry.
const idemConstraint = "uq_event_idem"

// appendCounter creates horopter.event.appends. It counts events submitted to AppendEvent,
// not changes; see appendOutcome.metricAttributes for what it is attributed by.
func appendCounter() metric.Int64Counter {
	counter, err := otel.Meter(telemetry.StoreScope).Int64Counter("horopter.event.appends",
		metric.WithUnit("{append}"),
		metric.WithDescription("Events submitted to the store, by outcome"))
	if err != nil {
		// The SDK returns a working instrument alongside a name-validation error, so the
		// store opens either way. The error means this call is wrong, not the environment.
		slog.Error("create the horopter.event.appends counter", "error", err)
	}
	return counter
}

// AppendEvent stores an event and folds it into the projection in one transaction.
//
// A duplicate idempotency key is not an error: CI retries are normal traffic, so the
// stored event is returned with created=false. That holds whether the retry arrives
// after the first attempt finished — the lookup below finds it — or while it is still
// in flight, which lands on the unique index instead; see recoverConcurrentDuplicate.
//
// lock_timeout is set as the transaction's FIRST statement, before anything that can
// wait. It used to be set inside lockProjectionWrite, which an append only reaches after
// insertEvent — so the INSERT's wait on uq_event_idem's speculative-insertion lock was
// unbounded, held only by the winning transaction's own lifetime. That is precisely the
// pinned-pool-connection hazard AppendLockTimeout exists to prevent, on the one statement
// it did not cover.
func (s *Store) AppendEvent(
	ctx context.Context, ev core.Event,
) (stored core.Event, created bool, err error) {
	ctx, span := tracer().Start(ctx, "AppendEvent")
	defer span.End()

	// submitted is the event as the caller sent it. The deferred record must NOT read the
	// ev that prepareEvent returns: on a validation failure prepareEvent returns a zero
	// core.Event, so tenant, id, type, source, subject and ref would all be blank on
	// exactly the error class these attributes exist to make debuggable.
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

// recordAppendOutcome emits AppendEvent's span attributes and counters, and marks the span
// failed when err is non-nil. Split out of AppendEvent's own deferred closure to keep
// AppendEvent within the project's cyclomatic complexity limit — the branch that gates
// horopter.refold on err == nil, as well as on out.outOfOrder, pushed it over on its own.
func (s *Store) recordAppendOutcome(
	ctx context.Context, span trace.Span, out appendOutcome, submitted core.Event, err error,
) {
	span.SetAttributes(out.attributes(submitted)...)
	s.appends.Add(ctx, 1, metric.WithAttributeSet(out.metricAttributes()))
	// Gated on err == nil as well as out.outOfOrder: applyToProjection sets out.outOfOrder
	// before it hands off to refoldOutOfOrderAppend, so a re-fold that ERRORS there still
	// reaches this call with out.outOfOrder true and out.refoldMoved at its zero value,
	// nil — movedSomethingRecorded(nil) is false, which would otherwise count a re-fold
	// that never finished as "recomputed from the log, nothing moved." s.appends has an
	// outcome=failed label for exactly this distinction; horopter.refold does not, and
	// gating the Add is the simpler way to stay honest without giving it one — see the
	// cardinality warning just below.
	if out.outOfOrder && err == nil {
		// Boolean, not the field list: a counter keeps a series per distinct attribute
		// set, and eight field names would give it 256.
		s.refolds.Add(ctx, 1, metric.WithAttributeSet(attribute.NewSet(
			attribute.Bool("horopter.refold.altered",
				movedSomethingRecorded(out.refoldMoved)))))
	}
	if err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, out.errKind)
	}
}

// prepareEvent validates the event and normalises the fields the schema constrains.
//
// A nil Attributes map and an empty one are semantically identical — both mean "no
// attributes" — but the attributes column is JSONB NOT NULL, and Postgres's DEFAULT only
// fires when a column is omitted from the INSERT list, never when NULL is bound
// explicitly, so a nil map reaches the constraint and fails. The NOT NULL constraint lives
// here, so this is where the contract is owned: normalising once at the store boundary
// covers every caller (ingress, CloudEvents mapping, a future egress seed, any other
// producer) instead of repairing them one at a time at their call sites.
// A store opened WithMaxEventAttributeBytes also refuses attributes over that size here,
// after validation, so the refusal is an invalid event like any other.
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

// findExistingDuplicate returns the event already stored under ev's idempotency key, if
// ev carries one and a stored event holds it. An event with no key can never be a
// duplicate, so it reports found=false without a round trip.
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

// writeEvent inserts the event, folds it into the projection and commits, all on tx.
//
// The 23505 arm is the concurrent-retry path: the check in findExistingDuplicate and this
// insert are not atomic, so two in-flight requests carrying one idempotency key both find
// nothing and both insert. The loser lands on the unique index and must be given the
// stored event rather than the constraint violation; see recoverConcurrentDuplicate.
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

// insertEvent writes the immutable event row. An empty idempotency key is bound as NULL:
// uq_event_idem is partial on idempotency_key IS NOT NULL, so binding "" instead would
// make every keyless event collide with every other one.
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

// isUniqueViolation reports whether err is the Postgres unique violation raised by the
// named constraint or unique index.
func isUniqueViolation(err error, constraint string) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) &&
		pgErr.Code == uniqueViolation && pgErr.ConstraintName == constraint
}

// isLockTimeout reports whether err is Postgres giving up on a lock wait because
// lock_timeout expired.
func isLockTimeout(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == lockNotAvailable
}

// setLockTimeout bounds how long any later statement in tx may wait for a lock.
//
// set_config(name, value, is_local => true) IS "SET LOCAL": it applies to this transaction
// and reverts when the transaction commits or rolls back — the same property that made
// pg_advisory_xact_lock the right choice over pg_advisory_lock, and the reason no code
// path has to remember to unset it. SET itself accepts no bind parameter, so set_config is
// how the Go constant stays the single source of truth for the value. lock_timeout reads a
// bare number as milliseconds.
//
// The bound covers everything the transaction does after this call and nothing before it,
// which is why both callers issue it as their transaction's first statement rather than
// alongside the advisory locks. The event INSERT waits on uq_event_idem's
// speculative-insertion lock, the row-level FOR UPDATE and the change upsert wait on
// another transaction, and the advisory locks wait on whoever holds them: pinning a pool
// connection on any of them is the same hazard, so the bound has to precede all of them.
func setLockTimeout(ctx context.Context, tx pgx.Tx, d time.Duration, what string) error {
	if _, err := tx.Exec(ctx, `SELECT set_config('lock_timeout', $1, true)`,
		strconv.FormatInt(d.Milliseconds(), 10)); err != nil {
		return fmt.Errorf("bound the %s lock wait at %s: %w", what, d, err)
	}
	return nil
}

// recoverConcurrentDuplicate returns the stored event after this caller lost a race for
// an idempotency key, which is the same result the non-concurrent duplicate path gives.
//
// Two in-flight requests carrying the same (tenant, idempotency_key) — a CI retry firing
// while the first attempt is still running — both find nothing at the check above and
// both insert. Duplicates are normal traffic, not an error condition, so the
// loser must not surface 23505 to its caller.
//
// The lookup runs on the pool, not on tx: Postgres aborts a transaction on any error, so
// no further statement can run on tx. That is also why the lookup is guaranteed to find
// the row — the unique index made the losing INSERT wait on the winning transaction and
// raise 23505 only once it had committed, so the winner's row is visible to the fresh
// snapshot this read takes.
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

// afterLoadHook is nil in production. Tests set it to widen the window between the
// projection read and its write, making the lost-update race deterministic instead
// of dependent on machine timing.
var afterLoadHook func()

// afterIdemLookupHook is nil in production. Tests set it to widen the window between
// the idempotency-key lookup and the event insert, making the concurrent-duplicate race
// deterministic instead of dependent on machine timing.
var afterIdemLookupHook func()

// afterDeleteHook is nil in production. Tests set it to hold a rebuild open between its
// DELETE and its re-insert, which is the window an append must not be able to enter.
var afterDeleteHook func()

// applyToProjection folds one event onto its change, under the advisory locks that make
// the read-modify-write a critical section.
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
	// ev.ID sorting below the change's current watermark is exactly the condition under
	// which an incremental fold and an id-ordered fold disagree — the divergence
	// RebuildProjection documents. It is resolved here, by recomputing the ref from the
	// log rather than by trying to patch the incremental rule, which is not achievable for
	// attributes.
	out.outOfOrder = found && ev.ID < cur.LastEventID
	if !out.outOfOrder {
		out.foldStatus = folded.Status
		out.foldClosed = folded.End != nil
		return upsertChange(ctx, tx, folded)
	}
	return refoldOutOfOrderAppend(ctx, tx, ev, folded, out)
}

// refoldOutOfOrderAppend recomputes ref from the log after an out-of-order append and
// records the re-folded change's status on out. Split out of applyToProjection to keep its
// branching within the complexity limit.
//
// folded is the incremental fold applyToProjection already computed and is about to
// reject; it is passed in, rather than recomputed, so this function can report what the
// re-fold actually moved. ref is recomputed from ev rather than threaded as a sixth
// parameter — core.RefFor is a pure string composition, not a lookup — to stay within the
// project's five-positional-parameter limit.
func refoldOutOfOrderAppend(
	ctx context.Context, tx pgx.Tx, ev core.Event, folded core.Change, out *appendOutcome,
) error {
	refRef := core.RefFor(ev)
	reref, err := refoldRef(ctx, tx, ev.Tenant, refRef)
	if err != nil {
		return err
	}
	// refoldRef already returns the change it wrote, so there is no second read here: no
	// second SELECT ... FOR UPDATE issued on the change row while this transaction already
	// holds it under the ref-exclusive advisory lock lockProjectionWrite took above.
	refolded := reref.change
	out.foldStatus = refolded.Status
	out.foldClosed = refolded.End != nil
	out.refoldMoved = alteredFields(folded, refolded)
	// Status and End are the pair that changes what the log SAYS HAPPENED, as against
	// which of two titles it kept. That is the one an operator wants without tracing on.
	// movedSomethingRecorded, not a second inline re-derivation of the same condition:
	// two independent expressions of "Status or End moved" is how they went out of sync
	// before.
	if movedSomethingRecorded(out.refoldMoved) {
		slog.WarnContext(ctx, "a re-fold moved a recorded outcome",
			"tenant", ev.Tenant, "ref", refRef.String(),
			"was", string(folded.Status), "now", string(refolded.Status),
			"moved", out.refoldMoved)
	}
	return nil
}

// lockProjectionWrite takes, in one round trip, the two advisory locks an append needs
// before it may read and rewrite a change row.
//
// The per-ref exclusive lock is what makes the fold correct. SELECT ... FOR UPDATE can
// only lock rows already visible to the transaction's snapshot, so when two transactions
// append the first events for a NEW ref they both see nothing, both compute Fold(nil, ev)
// and both run INSERT ... ON CONFLICT DO UPDATE; the second takes the DO UPDATE branch
// with its own EXCLUDED values, Postgres does not re-run the Go-side fold, and the first
// event's contribution is overwritten wholesale with no error raised. Locking the ref's
// *identity* rather than a row that may not exist yet removes that whole class of bug.
//
// The alternative considered and rejected was INSERT ... ON CONFLICT DO NOTHING followed
// by a re-SELECT ... FOR UPDATE: it closes the same race, but it costs a second round trip
// on every append and leaves the correctness resting on a retry loop that has to be right
// rather than on a lock that cannot be wrong. An advisory lock on the ref's identity is
// one statement and needs no retry.
//
// The tenant-scoped SHARED lock is the other half of RebuildProjection's exclusion. A
// rebuild deletes and re-inserts every row a tenant has, including refs no appender
// currently holds, so per-ref locks alone cannot keep the two apart: a rebuild would
// take no lock any appender waits on. Appenders share this lock with each other, so
// two appends to different refs still never contend on it; only a rebuild, which takes
// it exclusively, waits for them and makes them wait for it.
//
// pg_advisory_xact_lock, rather than pg_advisory_lock, matches Migrate and for the same
// reason: Postgres releases it when the transaction commits or rolls back, so there is
// no manual unlock call a cancelled context or a swallowed error can skip, leaving the
// lock held by a connection quietly returned to the pool.
//
// hashtext is 32-bit, so two unrelated refs can collide and briefly serialise against
// each other. That costs throughput, never correctness.
//
// The wait here is bounded by AppendLockTimeout — a rebuild holds the tenant lock
// exclusively for its whole duration, and an unbounded wait behind it pins a pool
// connection that other tenants need — but this function does not set that bound.
// AppendEvent does, as its transaction's first statement, so it covers the event INSERT
// as well as these locks. See AppendLockTimeout for the reasoning.
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

// lockAndLoadChange reads a change FOR UPDATE and reports found=false when the row does
// not exist yet.
//
// The advisory lock taken in lockProjectionWrite is what serialises the fold; FOR UPDATE
// alone cannot, because it can only lock a row the transaction can already see. It is
// kept as the row-level guard against a writer that has not taken the advisory lock —
// that lock is a convention this package must keep, not something the schema enforces.
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

// upsertChange writes a folded change. All three derived columns are absent from the column
// list, because Postgres generates them and rejects any value an INSERT supplies for one:
// end_effective from end_at (migration 0002), start_at from
// COALESCE(started_at, first_seen_at) (migration 0003), and locus from the ref's first
// segment (migration 0007). What this writes is the facts the domain owns — started_at,
// first_seen_at, end_at — so there is exactly one writer of each derived value and no second
// writer to drift from.
//
// None of the three timestamps is normalised to UTC on the way in. TIMESTAMPTZ stores an
// absolute instant, so a Go value's location never reaches the column and a .UTC() here
// would be cosmetic. Applying it to one half of the start fact and not the other spelled
// the two in adjacent lines as though the difference meant something.
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
