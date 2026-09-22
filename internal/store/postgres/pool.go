package postgres

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/exaring/otelpgx"
	"github.com/jackc/pgx/v5/pgxpool"
	semconv "go.opentelemetry.io/otel/semconv/v1.43.0"
)

// storeFromPool wraps an open pool and applies opts. The append counter is created here, from
// the global meter provider as it stands when the store opens — which is why every binary
// calls telemetry.Setup before opening a store.
func storeFromPool(pool *pgxpool.Pool, opts []Option) *Store {
	s := &Store{pool: pool, appends: appendCounter()}
	for _, opt := range opts {
		opt(s)
	}
	return s
}

// connect opens a pool for dsn and confirms it is reachable, closing the pool again if
// the ping fails. Open and OpenNoMigrate both need exactly this sequence before they
// diverge, so it lives here once rather than as two copies that a later change (a
// timeout, a retry, a pool setting) could silently update on only one path.
//
// connectErrHint is the caller-specific, operator-facing suggestion appended to a
// connect failure; the ping failure message is identical for both callers, so it is
// not parameterised.
func connect(ctx context.Context, dsn, connectErrHint string) (*pgxpool.Pool, error) {
	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		return nil, fmt.Errorf("parse the postgres dsn: %w; %s", err, connectErrHint)
	}
	// otelpgx traces each statement. Bound values are NOT included: they carry event
	// payloads, and query parameters are exactly where user data would leak into
	// telemetry. The statement text itself is this project's own SQL and is what makes
	// a span identifiable. poolTracer wraps it to report connection waits to egress.
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

// poolName names the pool on pgxpool.* metrics in place of otelpgx's default,
// host:port/database, which would publish the database's address and name on /metrics.
const poolName = "horopter"

// recordPoolStats exports the pool's statistics as pgxpool.* metrics through the global
// meter provider as it stands now. It runs only once the pool has answered a ping, so a
// pool that failed to connect registers nothing. A registration failure is logged: pool
// statistics are not worth refusing to open a store over.
func recordPoolStats(pool *pgxpool.Pool) {
	err := otelpgx.RecordStats(pool,
		otelpgx.WithStatsAttributes(semconv.DBClientConnectionPoolName(poolName)))
	if err != nil {
		slog.Error("record connection pool statistics; continuing without them", "error", err)
	}
}
