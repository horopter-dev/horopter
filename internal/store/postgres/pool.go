package postgres

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/exaring/otelpgx"
	"github.com/jackc/pgx/v5/pgxpool"
	semconv "go.opentelemetry.io/otel/semconv/v1.43.0"
)

// Counters come from the global meter provider, so call telemetry.Setup first.
func storeFromPool(pool *pgxpool.Pool, opts []Option) *Store {
	s := &Store{pool: pool, appends: appendCounter(), refolds: refoldCounter()}
	for _, opt := range opts {
		opt(s)
	}
	return s
}

// Shared by Open and OpenNoMigrate, so connection changes apply to both.
func connect(ctx context.Context, dsn, connectErrHint string) (*pgxpool.Pool, error) {
	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		return nil, fmt.Errorf("parse the postgres dsn: %w; %s", err, connectErrHint)
	}
	// Bound values are not traced: they carry event payloads.
	cfg.ConnConfig.Tracer = poolTracer{Tracer: otelpgx.NewTracer()}
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		return nil, fmt.Errorf("connect to postgres: %w; %s", err, connectErrHint)
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("ping postgres: %w; is the database reachable?", err)
	}
	recordPoolStats(pool)
	return pool, nil
}

// Replaces host:port/database, which would publish the database's address on /metrics.
const poolName = "horopter"

// Registration failures are logged: pool stats are not worth refusing to open over.
func recordPoolStats(pool *pgxpool.Pool) {
	err := otelpgx.RecordStats(pool,
		otelpgx.WithStatsAttributes(semconv.DBClientConnectionPoolName(poolName)))
	if err != nil {
		slog.Error("record connection pool statistics; continuing without them", "error", err)
	}
}
