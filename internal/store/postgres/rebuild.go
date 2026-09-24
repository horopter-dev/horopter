package postgres

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"horopter.dev/horopter/internal/core"
)

// RebuildLockTimeout bounds how long a rebuild waits for the tenant lock.
//
// It is longer than AppendLockTimeout because what a rebuild waits for is in-flight
// appends finishing — milliseconds each — so reaching this bound means the tenant is under
// sustained write load rather than that anything is wrong. Giving up then, with a message
// telling the operator to retry, beats queueing indefinitely behind a stream of appends.
const RebuildLockTimeout = 10 * time.Second

// lockRebuildWrite takes the tenant-scoped advisory lock exclusively, excluding every
// append for that tenant — see lockProjectionWrite for why the granularity is the tenant
// and not the ref.
//
// The wait is bounded by RebuildLockTimeout so a rebuild that cannot get the lock promptly
// gives up and says so, rather than queueing behind a stream of appends while holding a
// connection of its own.
//
// Setting that bound is still the FIRST thing the rebuild transaction does, because this
// call is RebuildProjection's first statement after Begin — the property AppendEvent had
// to hoist setLockTimeout out of lockProjectionWrite to obtain, and which a rebuild
// already had. Nothing may be added to RebuildProjection above this call.
func lockRebuildWrite(ctx context.Context, tx pgx.Tx, tenantID string) error {
	if err := setLockTimeout(ctx, tx, RebuildLockTimeout, "rebuild"); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx,
		`SELECT pg_advisory_xact_lock(hashtext($1))`, tenantID); err != nil {
		if isLockTimeout(err) {
			return fmt.Errorf(
				"gave up after %s waiting for the rebuild lock on tenant %q: %w; appends "+
					"for this tenant are holding it, so nothing was written — retry when "+
					"the tenant's write traffic is quieter",
				RebuildLockTimeout, tenantID, err)
		}
		return fmt.Errorf(
			"take rebuild lock for tenant %q: %w; nothing was written, so the rebuild "+
				"can be retried", tenantID, err)
	}
	return nil
}

// RebuildProjection discards a tenant's projection and replays its event log in ULID
// order, holding the tenant's advisory lock exclusively so no append can interleave
// with the delete-and-reinsert.
//
// Without that lock an appender blocked on a row this transaction deleted re-checks,
// finds nothing, folds from nil and overwrites the rebuilt row — the same lost-update
// mechanism the per-ref lock exists to stop. A per-ref lock is the wrong granularity
// here because a rebuild touches refs no appender is holding, including refs that do
// not exist yet; see lockProjectionWrite for how the two locks compose.
//
// What it reproduces is the id-ordered fold, which is not always the projection
// production holds. Live appends fold in COMMIT order — the order in which append
// transactions take the per-ref advisory lock — and a rebuild folds in id order. The two
// diverge for ANY pair of same-ref events whose commit order differs from their id order.
// No sub-millisecond coincidence is needed for that, because an event's id is minted from
// s.clock() on the receiving ingress replica BEFORE its append transaction starts, and
// nothing afterwards keeps the two orders aligned:
//
//   - Across replicas the divergence window is the CLOCK SKEW between them, not one
//     millisecond. A deployment.completed handled by replica B can carry a lower id than a
//     deployment.started handled by replica A that arrived earlier, and then folds second.
//   - Within a single replica the per-ref advisory lock itself reorders: a request that
//     minted a lower id can lose the race for the lock while one holding a higher id folds
//     first. The window is everything between minting the id and taking that lock — Begin,
//     the idempotency lookup, the event INSERT, and the lock wait, which AppendLockTimeout
//     bounds at seconds.
//   - Only then, as the narrowest of the three, does entropy matter: core.NewULID draws
//     from crypto/rand rather than ulid.MonotonicEntropy, so two ids minted inside one
//     millisecond do not even tie-break by generation order.
//
// A rebuild can therefore produce a different Subject, Type, Title, Actor, attribute set,
// LastEventID, Source, End or Status than the one live folding produced, under four
// different rules. Subject, Type, LastEventID and Source are assigned unconditionally:
// "the last event folded wins". Title and Actor are guarded non-empty, so their rule is
// "the last NON-EMPTY event folded wins" — a reorder still moves them between two events
// that both carry one, but an event carrying none cannot blank what a previous event
// recorded, which is why the guard exists. Attributes MERGE, so the set of keys is a
// union and commutative — no reorder loses or gains one — while the value at a key that
// two events both set follows last-wins like the first group; a query filtering on such
// a key can therefore match a different set of changes afterwards. End and Status follow
// the fourth — core.Fold guards both on End being nil, so once a change has closed only
// a terminal event moves them: "the last TERMINAL event folded wins".
// Reordering two terminal events swaps the recorded outcome; a late annotation cannot
// reopen a change that already closed. Start, StartedAt and FirstSeenAt are NOT in that set:
// both start facts are minima (migration 0003, core.Fold), so they are commutative and the
// two orders agree on them. Every event is still folded in, and the rebuild is still
// deterministic — replaying the same log again gives the same answer — but "reproduces the
// projection exactly" is only true up to that ordering.
//
// This is reachable by following the documentation: --rebuild-tenant is the README's
// stated remedy for migration 0003, so an operator running it can flip a change's recorded
// outcome from completed to failed, or back. The remedy, deferred to v0.2 and deliberately
// not attempted here, is to make every field's rule commutative or to detect ev.ID <
// cur.LastEventID during an append and re-fold that ref from the log. Until then the
// README's rebuild section carries the same caveat, where an operator will actually meet
// it.
//
// Operational exposure, which v0.1 does not remove: the exclusive lock blocks every
// append for this tenant for the rebuild's whole duration. Blocked appends fail after
// AppendLockTimeout with a retryable error rather than pinning pool connections, so the
// bound on an event's delay is the producer's own retry patience. There is no batching,
// no progress reporting and no online rebuild.
func (s *Store) RebuildProjection(ctx context.Context, tenantID string) (err error) {
	ctx, span := tracer().Start(ctx, "RebuildProjection")
	defer span.End()

	// Recorded from a defer, not after the replay, so the failure an operator actually
	// meets is covered: a rebuild that gives up in lockRebuildWrite on RebuildLockTimeout
	// used to produce a span saying neither which tenant it was for nor how long it
	// waited. The counters stay zero on that path, which is the truth — nothing was
	// replayed.
	var (
		lockWait time.Duration
		events   int
		refs     int
	)
	defer func() {
		span.SetAttributes(
			attribute.String("horopter.rebuild.tenant", tenantID),
			attribute.Int("horopter.rebuild.events", events),
			attribute.Int("horopter.rebuild.refs", refs),
			attribute.Int64("horopter.rebuild.lock_wait_ms", lockWait.Milliseconds()),
		)
		if err != nil {
			span.RecordError(err)
			span.SetStatus(codes.Error, "")
		}
	}()

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin rebuild transaction: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	// lockWait is assigned before the error is checked, so a lock that timed out reports
	// the wait that ended in the timeout rather than zero.
	lockStart := time.Now()
	lockErr := lockRebuildWrite(ctx, tx, tenantID)
	lockWait = time.Since(lockStart)
	if lockErr != nil {
		return lockErr
	}
	if err := clearProjection(ctx, tx, tenantID); err != nil {
		return err
	}

	events, refs, err = replayProjection(ctx, tx, tenantID)
	if err != nil {
		return err
	}

	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit rebuild for tenant %q: %w", tenantID, err)
	}
	return nil
}

// replayProjection folds a tenant's whole event log in id order and writes the result,
// returning the number of events replayed and the number of refs they folded into.
//
// Kept out of RebuildProjection so that function stays legible while it carries the lock,
// the clear, the span and the commit — and so it has complexity headroom, which it does
// not otherwise have.
// The results are NAMED because they are two bare ints in a row: nothing catches a
// transposition at either end, and TestRebuildRecordsItsShapeOnASpan seeds two events
// onto one ref specifically so the two values differ and a swap is visible.
func replayProjection(
	ctx context.Context, tx pgx.Tx, tenantID string,
) (events, refs int, err error) {
	acc, events, err := foldEventLog(ctx, tx, tenantID)
	if err != nil {
		return 0, 0, err
	}
	if err := writeProjection(ctx, tx, acc); err != nil {
		return 0, 0, err
	}
	return events, len(acc), nil
}

// clearProjection discards a tenant's projection inside the rebuild transaction. The hook
// is what lets a test hold the rebuild open in the window an append must not enter.
func clearProjection(ctx context.Context, tx pgx.Tx, tenantID string) error {
	if _, err := tx.Exec(ctx, `DELETE FROM change WHERE tenant = $1`, tenantID); err != nil {
		return fmt.Errorf("clear projection for tenant %q: %w", tenantID, err)
	}
	if afterDeleteHook != nil {
		afterDeleteHook()
	}
	return nil
}

// foldEventLog replays a tenant's event log in ULID order and returns the projection it
// implies, keyed by ref, together with the number of events it read to build it.
//
// Folding inside the scan loop, rather than materialising the whole log into a slice
// first, keeps peak memory proportional to the number of distinct refs rather than the
// total number of events replayed.
//
// The accumulator passes a nil *core.Change for the first event of each ref, matching how
// AppendEvent applies events incrementally; core.Fold is written so both spellings
// converge.
//
// The SELECT names only the columns Fold reads, which is why origin and edited are absent
// — but locus is NOT one of those omissions. core.RefFor composes a point change's ref
// from it, so dropping it from this list rebuilds every point change under "/<id>" and
// silently diverges from the projection AppendEvent wrote, which is the shape of the bug
// 0004 shipped.
func foldEventLog(
	ctx context.Context, tx pgx.Tx, tenantID string,
) (map[core.Ref]core.Change, int, error) {
	rows, err := tx.Query(ctx, `
		SELECT id, tenant, schema_version, source, type, subject, actor, title,
		       occurred_at, received_at, correlation_ref, attributes, locus
		FROM event WHERE tenant = $1 ORDER BY id ASC`, tenantID)
	if err != nil {
		return nil, 0, fmt.Errorf("read event log for tenant %q: %w", tenantID, err)
	}
	defer rows.Close()

	acc := map[core.Ref]core.Change{}
	events := 0
	for rows.Next() {
		var (
			ev    core.Event
			corr  string
			locus string
		)
		if err := rows.Scan(&ev.ID, &ev.Tenant, &ev.SchemaVersion, &ev.Source, &ev.Type,
			&ev.Subject, &ev.Actor, &ev.Title, &ev.OccurredAt, &ev.ReceivedAt,
			&corr, &ev.Attributes, &locus); err != nil {
			return nil, 0, fmt.Errorf("scan event row for tenant %q: %w", tenantID, err)
		}
		ev.CorrelationRef = core.RefFromRow(corr)
		ev.Locus = core.LocusFromRow(locus)
		events++
		ref := core.RefFor(ev)
		var prev *core.Change
		if c, ok := acc[ref]; ok {
			prev = &c
		}
		acc[ref] = core.Fold(prev, ev)
	}
	if err := rows.Err(); err != nil {
		return nil, 0, fmt.Errorf("iterate event log for tenant %q: %w", tenantID, err)
	}
	return acc, events, nil
}

// writeProjection inserts every rebuilt change.
func writeProjection(ctx context.Context, tx pgx.Tx, acc map[core.Ref]core.Change) error {
	for _, c := range acc {
		if err := upsertChange(ctx, tx, c); err != nil {
			return err
		}
	}
	return nil
}
