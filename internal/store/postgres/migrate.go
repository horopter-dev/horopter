package postgres

import (
	"context"
	"embed"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

//go:embed migrations/*.sql
var migrationFS embed.FS

// migrationLockKey is an arbitrary but fixed application id for pg_advisory_xact_lock.
// Every process that migrates uses this same key, which is what serialises them.
const migrationLockKey int64 = 8145209847321

// AppliedMigrations names the migrations THIS Open applied, in the order it applied them.
// It is empty when the schema was already current, and always empty from OpenNoMigrate.
//
// The applied_migration ledger cannot answer this after the fact: once the transaction
// commits, a file applied by this run and one applied a year ago are both simply present.
// horoctl-migrate reports the difference to an operator, so the difference has to be
// carried out of the run that knew it.
func (s *Store) AppliedMigrations() []string { return s.appliedMigrations }

// Migrate applies the embedded migrations a database has not seen yet, inside one
// transaction, guarded by a transaction-scoped advisory lock, and returns the names it
// applied — empty when the database was already current.
//
// The lock is what makes this safe when several daemons start at once, as they do on
// every rolling deploy: without it, concurrent CREATE INDEX statements race and can
// leave a partially applied schema with no error surfaced to the operator.
//
// pg_advisory_xact_lock, rather than pg_advisory_lock, is deliberate: it is released
// automatically by Postgres when the transaction commits or rolls back, so there is no
// manual unlock call that can be skipped by a cancelled context or a swallowed error and
// leave the lock held by a connection quietly returned to the pool. Postgres DDL is
// transactional, so wrapping every migration statement in the same transaction is also
// what makes a mid-file failure roll back cleanly instead of leaving a partially applied
// schema.
//
// applied_migration is the ledger that makes "has not seen yet" meaningful. Without it
// every file re-executes on every run, which happens to be harmless only while every
// migration is written with IF NOT EXISTS; the first backfill UPDATE, RENAME COLUMN or
// data transformation would either error on the second run or, in a system of record,
// silently corrupt data by re-applying. A database migrated before the ledger existed
// simply has an empty one, so every file re-executes once and is recorded thereafter.
//
// That recovery is safe for 0001 alone, and only because every statement in it is written
// IF NOT EXISTS. 0001 is also the only file that predates the ledger — 0002 shipped in the
// same commit as applied_migration, so no database has ever held 0002 without a ledger
// row for it. Neither 0002 nor 0003 is idempotent: 0002's ADD CONSTRAINT
// change_status_vocabulary fails outright on a second run, which is what
// TestMigrationLedgerAppliesEachFileOnce demonstrates. So restoring a backup taken before
// the ledger, or dropping applied_migration to "start again", produces a database whose
// migration cannot start: the operator has to insert a row for every file the schema
// already carries before running this again, not let them replay.
func Migrate(ctx context.Context, pool *pgxpool.Pool) ([]string, error) {
	tx, err := pool.Begin(ctx)
	if err != nil {
		return nil, fmt.Errorf("begin migration transaction: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock($1)`, migrationLockKey); err != nil {
		return nil, fmt.Errorf("take migration advisory lock: %w", err)
	}
	if _, err := tx.Exec(ctx, `
		CREATE TABLE IF NOT EXISTS applied_migration (
		    filename   TEXT PRIMARY KEY,
		    applied_at TIMESTAMPTZ NOT NULL DEFAULT now()
		)`); err != nil {
		return nil, fmt.Errorf("create the applied_migration ledger: %w", err)
	}

	names, err := migrationNames()
	if err != nil {
		return nil, err
	}
	applied, err := applyPendingMigrations(ctx, tx, names)
	if err != nil {
		return nil, err
	}
	if err := recordSchemaVersion(ctx, tx); err != nil {
		return nil, err
	}

	// Reported only past this point. Every file ran inside the transaction above, so a
	// commit that fails applied nothing — and a caller told otherwise would log DDL that
	// the database rolled back.
	if err := tx.Commit(ctx); err != nil {
		return nil, fmt.Errorf("commit migration transaction: %w", err)
	}
	return applied, nil
}

// migrationNames lists the embedded migration filenames in the order they must run.
func migrationNames() ([]string, error) {
	entries, err := migrationFS.ReadDir("migrations")
	if err != nil {
		return nil, fmt.Errorf("read embedded migrations: %w", err)
	}
	if len(entries) == 0 {
		return nil, errors.New(
			"no embedded migrations found; migrations/*.sql must ship inside the binary")
	}
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		names = append(names, e.Name())
	}
	sort.Strings(names)
	return names, nil
}

// readLedger returns the set of migration filenames the database has already applied.
func readLedger(ctx context.Context, tx pgx.Tx) (map[string]bool, error) {
	rows, err := tx.Query(ctx, `SELECT filename FROM applied_migration`)
	if err != nil {
		return nil, fmt.Errorf("read the applied_migration ledger: %w", err)
	}
	defer rows.Close()

	applied := map[string]bool{}
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return nil, fmt.Errorf("scan the applied_migration ledger: %w", err)
		}
		applied[name] = true
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate the applied_migration ledger: %w", err)
	}
	return applied, nil
}

// applyPendingMigrations executes, in order, every named migration the ledger does not
// already record, adding each to the ledger as it goes, and returns the names it ran.
func applyPendingMigrations(ctx context.Context, tx pgx.Tx, names []string) ([]string, error) {
	applied, err := readLedger(ctx, tx)
	if err != nil {
		return nil, err
	}

	ran := []string{}
	for _, name := range names {
		if applied[name] {
			continue
		}
		stmt, err := migrationFS.ReadFile("migrations/" + name)
		if err != nil {
			return nil, fmt.Errorf("read migration %q: %w", name, err)
		}
		if _, err := tx.Exec(ctx, string(stmt)); err != nil {
			return nil, fmt.Errorf("apply migration %q: %w", name, err)
		}
		if _, err := tx.Exec(ctx,
			`INSERT INTO applied_migration (filename) VALUES ($1)`, name); err != nil {
			return nil, fmt.Errorf("record migration %q in the ledger: %w", name, err)
		}
		ran = append(ran, name)
	}
	return ran, nil
}

// recordSchemaVersion writes the version implied by the highest migration filename the
// LEDGER holds. It must run after applyPendingMigrations, inside the same transaction, so
// it reads the ledger this run has just finished extending.
//
// The ledger is the authority, not the binary's embedded list. An older horoctl-migrate
// run against a newer database embeds fewer migrations, so recording its own highest
// embedded file rewrote schema_version DOWNWARD while applied_migration still listed every
// file actually applied — leaving the recorded version contradicting the database's real
// structure, with nothing raised. Reading the ledger makes that binary record the true,
// higher version instead, so its own ExpectedSchemaVersion check fails loudly and the
// operator learns they are running the wrong binary.
//
// Deriving the number at all, rather than hardcoding it, is what stops a second kind of
// drift: the version used to be a literal INSERT inside 0001_init.sql, so adding a
// migration without editing that literal left the database claiming a version the schema
// no longer matched. ExpectedSchemaVersion stays a Go constant precisely so a daemon can
// assert against this value and refuse to run on a schema it does not understand.
func recordSchemaVersion(ctx context.Context, tx pgx.Tx) error {
	var latest *string
	if err := tx.QueryRow(ctx,
		`SELECT max(filename) FROM applied_migration`).Scan(&latest); err != nil {
		return fmt.Errorf("read the highest applied migration from the ledger: %w", err)
	}
	if latest == nil {
		return errors.New(
			"the applied_migration ledger is empty after applying migrations, so no " +
				"schema version can be derived; migrationNames rejects an empty embedded " +
				"set, so this should be unreachable — report it rather than working around it")
	}
	v, err := schemaVersionFromFilename(*latest)
	if err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `
		INSERT INTO schema_version (version) VALUES ($1)
		ON CONFLICT (singleton) DO UPDATE SET version = $1, applied_at = now()`, v); err != nil {
		return fmt.Errorf("record schema version %d: %w", v, err)
	}
	return nil
}

// schemaVersionFromFilename reads the leading digits of a migration filename.
func schemaVersionFromFilename(name string) (int, error) {
	digits := name
	if i := strings.IndexFunc(name, func(r rune) bool { return r < '0' || r > '9' }); i >= 0 {
		digits = name[:i]
	}
	v, err := strconv.Atoi(digits)
	if err != nil || v <= 0 {
		return 0, fmt.Errorf(
			"migration %q has no positive numeric prefix; name migrations "+
				"NNNN_description.sql so the schema version can be derived from them", name)
	}
	return v, nil
}
