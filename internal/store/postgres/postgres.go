// Package postgres implements Horopter's persistence on PostgreSQL.
package postgres

import (
	"context"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/trace"
	"horopter.dev/horopter/internal/telemetry"
)

// Both pgx.Tx and the pool, so findByIdemKey also works after a unique violation aborts tx.
type querier interface {
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

// Store is the PostgreSQL-backed event store.
type Store struct {
	pool              *pgxpool.Pool
	appends           metric.Int64Counter
	refolds           metric.Int64Counter
	maxAttributeBytes int
	appliedMigrations []string
}

// Option configures a Store as it opens.
type Option func(*Store)

// WithMaxEventAttributeBytes makes AppendEvent refuse events whose attributes exceed n bytes.
func WithMaxEventAttributeBytes(n int) Option {
	return func(s *Store) { s.maxAttributeBytes = n }
}

// Pool exposes the connection pool for tests and for callers needing raw access.
func (s *Store) Pool() *pgxpool.Pool { return s.pool }

// Close releases the pool.
func (s *Store) Close() { s.pool.Close() }

// Store spans cover the public operations; otelpgx's per-statement spans are separate.
func tracer() trace.Tracer { return otel.Tracer(telemetry.StoreScope) }

// Open connects to dsn and applies all migrations.
func Open(ctx context.Context, dsn string, opts ...Option) (*Store, error) {
	pool, err := connect(ctx, dsn, "check the dsn is a valid postgres connection string")
	if err != nil {
		return nil, err
	}
	applied, err := Migrate(ctx, pool)
	if err != nil {
		pool.Close()
		return nil, err
	}
	s := storeFromPool(pool, opts)
	s.appliedMigrations = applied
	return s, nil
}
