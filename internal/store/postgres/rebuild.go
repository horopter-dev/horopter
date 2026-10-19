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

// RebuildLockTimeout bounds the rebuild's tenant-lock wait; hitting it means sustained writes.
const RebuildLockTimeout = 10 * time.Second

// Must stay RebuildProjection's first statement, so the lock timeout covers everything.
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

// RebuildProjection replays a tenant's log in id order, exclusively locked; see known-limits.md.
func (s *Store) RebuildProjection(ctx context.Context, tenantID string) (err error) {
	ctx, span := tracer().Start(ctx, "RebuildProjection")
	defer span.End()

	// Deferred, so a rebuild that times out on the lock still records its tenant and wait.
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

	// Assigned before the error check, so a timed-out lock reports its wait.
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

// Named results: two bare ints are easy to transpose.
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

func clearProjection(ctx context.Context, tx pgx.Tx, tenantID string) error {
	if _, err := tx.Exec(ctx, `DELETE FROM change WHERE tenant = $1`, tenantID); err != nil {
		return fmt.Errorf("clear projection for tenant %q: %w", tenantID, err)
	}
	if afterDeleteHook != nil {
		afterDeleteHook()
	}
	return nil
}

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
