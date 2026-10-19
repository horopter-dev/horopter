package postgres

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// ApplyMigrationsBefore applies and records every migration before cutoff, as Migrate does.
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

// SetAfterLoadHook installs fn between applyToProjection's locked read and its write.
func SetAfterLoadHook(fn func()) { afterLoadHook = fn }

// SetAfterIdemLookupHook installs fn between an empty idempotency lookup and the insert.
func SetAfterIdemLookupHook(fn func()) { afterIdemLookupHook = fn }

// SetAfterDeleteHook installs fn between a rebuild's clear and its re-insert.
func SetAfterDeleteHook(fn func()) { afterDeleteHook = fn }

// AttributesOf returns q's attribute filters, unexported so the allowlist cannot be bypassed.
func AttributesOf(q ChangeQuery) map[string]string { return q.attributes }

// ChangeColumns exposes the columns a change read returns, so a test sizes rows as reads do.
const ChangeColumns = changeColumns
