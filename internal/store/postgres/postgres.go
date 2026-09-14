// Package postgres implements Horopter's persistence on PostgreSQL.
//
// Postgres is the only backend. Timestamps are native timestamptz, so ordering and
// range comparison are temporal rather than lexicographic, and attributes are JSONB.
package postgres

import (
	"context"
	"embed"
	"fmt"
	"sort"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

//go:embed migrations/*.sql
var migrationFS embed.FS

// migrationLockKey is an arbitrary but fixed application id for pg_advisory_xact_lock.
// Every process that migrates uses this same key, which is what serialises them.
const migrationLockKey int64 = 8145209847321

// Store is the PostgreSQL-backed event store.
type Store struct{ pool *pgxpool.Pool }

// Pool exposes the connection pool for tests and for callers needing raw access.
func (s *Store) Pool() *pgxpool.Pool { return s.pool }

// Close releases the pool.
func (s *Store) Close() { s.pool.Close() }

// Open connects to dsn and applies all migrations.
func Open(ctx context.Context, dsn string) (*Store, error) {
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		return nil, fmt.Errorf("connect to postgres: %w; check the dsn is a valid "+
			"postgres connection string", err)
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("ping postgres: %w; is the database reachable?", err)
	}
	if err := Migrate(ctx, pool); err != nil {
		pool.Close()
		return nil, err
	}
	return &Store{pool: pool}, nil
}

// Migrate applies every embedded migration inside one transaction, guarded by a
// transaction-scoped advisory lock.
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
func Migrate(ctx context.Context, pool *pgxpool.Pool) error {
	tx, err := pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin migration transaction: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock($1)`, migrationLockKey); err != nil {
		return fmt.Errorf("take migration advisory lock: %w", err)
	}

	entries, err := migrationFS.ReadDir("migrations")
	if err != nil {
		return fmt.Errorf("read embedded migrations: %w", err)
	}
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		names = append(names, e.Name())
	}
	sort.Strings(names)

	for _, name := range names {
		stmt, err := migrationFS.ReadFile("migrations/" + name)
		if err != nil {
			return fmt.Errorf("read migration %q: %w", name, err)
		}
		if _, err := tx.Exec(ctx, string(stmt)); err != nil {
			return fmt.Errorf("apply migration %q: %w", name, err)
		}
	}

	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit migration transaction: %w", err)
	}
	return nil
}

// ChangeQuery selects changes overlapping a time window.
type ChangeQuery struct {
	Tenant  string
	From    time.Time
	To      time.Time
	Subject string
	Type    string
	Status  string
	Limit   int
}

// EventQuery selects raw events for audit, export, and debugging.
type EventQuery struct {
	Tenant string
	Ref    string
	Limit  int
}
