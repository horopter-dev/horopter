package postgres

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// ApplyMigrationsBefore applies every embedded migration sorting strictly before cutoff,
// recording each in the applied_migration ledger exactly as Migrate does, so a later
// Migrate call on the same pool applies only cutoff and whatever follows it.
//
// It exists so a test can seed old-shape rows under an old schema before exercising the
// migration that changes that shape, without duplicating migrate.go's ledger bookkeeping.
func ApplyMigrationsBefore(ctx context.Context, pool *pgxpool.Pool, cutoff string) error {
	tx, err := pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	if _, err := tx.Exec(ctx, `
		CREATE TABLE IF NOT EXISTS applied_migration (
		    filename   TEXT PRIMARY KEY,
		    applied_at TIMESTAMPTZ NOT NULL DEFAULT now()
		)`); err != nil {
		return fmt.Errorf("create the applied_migration ledger: %w", err)
	}

	names, err := migrationNames()
	if err != nil {
		return err
	}
	for _, name := range names {
		if name >= cutoff {
			continue
		}
		if err := applyOneMigration(ctx, tx, name); err != nil {
			return err
		}
	}
	return tx.Commit(ctx)
}

// applyOneMigration executes one embedded migration file and records it in the ledger,
// both within the caller's transaction.
func applyOneMigration(ctx context.Context, tx pgx.Tx, name string) error {
	stmt, err := migrationFS.ReadFile("migrations/" + name)
	if err != nil {
		return fmt.Errorf("read migration %q: %w", name, err)
	}
	if _, err := tx.Exec(ctx, string(stmt)); err != nil {
		return fmt.Errorf("apply migration %q: %w", name, err)
	}
	if _, err := tx.Exec(ctx,
		`INSERT INTO applied_migration (filename) VALUES ($1)`, name); err != nil {
		return fmt.Errorf("record migration %q in the ledger: %w", name, err)
	}
	return nil
}

// SetAfterLoadHook installs fn as the hook applyToProjection calls between its locked
// read of a change and the write of the folded result, or clears it when fn is nil.
//
// This file is part of the test binary only, so afterLoadHook stays unreachable from
// outside the package in a normal build.
func SetAfterLoadHook(fn func()) { afterLoadHook = fn }

// SetAfterIdemLookupHook installs fn as the hook AppendEvent calls after an
// idempotency-key lookup that found nothing and before the event insert, or clears it
// when fn is nil. It is what lets a test hold two appends open across that window so
// both reach the insert, which is the only way to exercise the real unique-violation
// recovery rather than a faked error.
func SetAfterIdemLookupHook(fn func()) { afterIdemLookupHook = fn }

// SetAfterDeleteHook installs fn as the hook RebuildProjection calls between clearing a
// tenant's projection and re-inserting it, or clears it when fn is nil. Holding a
// rebuild open there is how a test puts an append into exactly the window the
// tenant-scoped advisory lock exists to close.
func SetAfterDeleteHook(fn func()) { afterDeleteHook = fn }

// AttributesOf returns q's attribute filters. The field is unexported so that
// NewChangeQuery's allowlist check cannot be bypassed by assigning it after
// construction, which would silently return every row rather than refusing the key.
func AttributesOf(q ChangeQuery) map[string]string { return q.attributes }

// ChangeColumns exposes the columns a change read returns, so a test sizes rows as reads do.
const ChangeColumns = changeColumns
