// Package postgres implements Horopter's persistence on PostgreSQL.
//
// Postgres is the only backend. Timestamps are native timestamptz, so ordering and
// range comparison are temporal rather than lexicographic, and attributes are JSONB.
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

// querier is the QueryRow subset that both pgx.Tx and *pgxpool.Pool provide, so
// findByIdemKey can run either inside the append transaction or on the pool once a
// unique violation has aborted that transaction.
type querier interface {
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

// Store is the PostgreSQL-backed event store.
type Store struct {
	pool              *pgxpool.Pool
	appends           metric.Int64Counter
	maxAttributeBytes int
	appliedMigrations []string
}

// Option configures a Store as it opens.
type Option func(*Store)

// WithMaxEventAttributeBytes makes AppendEvent refuse, as an invalid event, one whose
// attributes are larger than n bytes of JSON as core.Event.CheckAttributeSize measures them.
// A store opened without it applies no cap: horoctl-migrate never appends, and
// horopter-ingress passes the cap its operator configured.
func WithMaxEventAttributeBytes(n int) Option {
	return func(s *Store) { s.maxAttributeBytes = n }
}

// Pool exposes the connection pool for tests and for callers needing raw access.
func (s *Store) Pool() *pgxpool.Pool { return s.pool }

// Close releases the pool.
func (s *Store) Close() { s.pool.Close() }

// tracer returns the store's own instrumentation scope. Spans from this scope are the
// four public operations below; per-statement spans come from otelpgx under its own
// scope, so the two are separately switchable.
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
