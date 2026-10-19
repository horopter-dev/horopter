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

// Fixed and shared by every migrating process, which serialises them.
const migrationLockKey int64 = 8145209847321

// AppliedMigrations names what this Open applied; the ledger cannot tell that afterwards.
func (s *Store) AppliedMigrations() []string { return s.appliedMigrations }

// Migrate applies unseen migrations in one transaction under an advisory lock.
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

	// Reported only after commit: a failed commit applied nothing.
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

// A pre-ledger backup needs ledger rows inserted by hand: 0002 and 0003 are not idempotent.
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

// Reads the ledger, not the embedded list, so an old binary records the true, higher version.
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
