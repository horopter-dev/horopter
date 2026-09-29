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
// What it reproduces is the id-ordered fold. From this version on, that is also what a
// live append converges to: applyToProjection (append.go) detects an event whose id sorts
// below the change's current watermark and re-folds that ref from the log instead of
// folding the event in incrementally, so a rebuild finds nothing left to change for any
// ref appended to entirely under this version.
//
// An append still arrives out of order for the same reasons it always could — an event's
// id is minted from s.clock() on the receiving ingress replica BEFORE its append
// transaction starts, and nothing afterwards keeps the two orders aligned:
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
// What changed is the consequence, not the frequency: an out-of-order append now re-folds
// its ref instead of letting the incremental answer stand, so live folding and a rebuild
// agree for any ref appended to entirely under this version.
//
// The watermark the out-of-order check compares against, cur.LastEventID, is the ref's
// true maximum id only if the ref has never folded out of id order before. A ref whose
// drift predates this version holds a watermark below its real maximum, so a later append
// whose id sits above the watermark but below that true maximum still takes the in-order
// arm, and the pre-existing divergence survives untouched. --rebuild-tenant remains the
// remedy for that case: a rebuild always replays the whole log in id order regardless of
// what the watermark says.
//
// For a ref whose drift predates this version, a rebuild can still produce a different
// Subject, Type, Title, Actor, attribute set, LastEventID, End or Status than what live
// folding holds, under four different rules. Subject, Type and LastEventID are assigned
// unconditionally: "the last event folded wins". Title and Actor are guarded non-empty, so
// their rule is "the last NON-EMPTY event folded wins" — a reorder still moves them
// between two events that both carry one, but an event carrying none cannot blank what a
// previous event recorded, which is why the guard exists. Attributes MERGE, so the set of
// keys is a union and commutative — no reorder loses or gains one — while the value at a
// key that two events both set follows last-wins like the first group; a query filtering
// on such a key can therefore match a different set of changes afterwards. End and Status
// follow the fourth — core.Fold guards both on End being nil, so once a change has closed
// only a terminal event moves them: "the last TERMINAL event folded wins".
// Reordering two terminal events swaps the recorded outcome; a late annotation cannot
// reopen a change that already closed. Start, StartedAt, FirstSeenAt and the source set are
// NOT in that set: both start facts are minima (migration 0003, core.Fold) and the source
// set is a union, so all of them are commutative and the two orders agree on them. Every
// event is still folded in, and the rebuild is still deterministic — replaying the same log
// again gives the same answer — but "reproduces the projection exactly" is only true up to
// that ordering.
//
// See README.md's "Rebuilding a tenant's projection" section for what an operator meets
// today.
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
	acc, err := foldEventLog(ctx, tx, tenantID)
	if err != nil {
		return 0, 0, err
	}
	for _, f := range acc {
		events += f.events
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
// implies, keyed by ref, each entry carrying the change folded for that ref together with
// the number of events that produced it.
func foldEventLog(
	ctx context.Context, tx pgx.Tx, tenantID string,
) (map[core.Ref]foldedRef, error) {
	rows, err := tx.Query(ctx, foldEventQuery(`tenant = $1`), tenantID)
	if err != nil {
		return nil, fmt.Errorf("read event log for tenant %q: %w", tenantID, err)
	}
	defer rows.Close()

	acc, err := foldRows(rows)
	if err != nil {
		return nil, fmt.Errorf("fold event log for tenant %q: %w", tenantID, err)
	}
	return acc, nil
}

// writeProjection inserts every rebuilt change.
func writeProjection(ctx context.Context, tx pgx.Tx, acc map[core.Ref]foldedRef) error {
	for _, f := range acc {
		if err := upsertChange(ctx, tx, f.change); err != nil {
			return err
		}
	}
	return nil
}
