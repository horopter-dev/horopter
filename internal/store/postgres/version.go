package postgres

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5/pgxpool"
)

// ExpectedSchemaVersion is the schema this build understands. Ingress and egress ship
// as separate images and can therefore be deployed at different versions; each asserts
// this at startup so a skewed rollout fails loudly instead of corrupting data quietly.
const ExpectedSchemaVersion = 1

// SchemaVersion reads the version recorded by the migration.
func (s *Store) SchemaVersion(ctx context.Context) (int, error) {
	var v int
	if err := s.pool.QueryRow(ctx, `SELECT version FROM schema_version`).Scan(&v); err != nil {
		return 0, fmt.Errorf("read schema_version: %w; has horopter-migrate been run?", err)
	}
	return v, nil
}

// AssertSchemaVersion refuses to continue against an unexpected schema.
func (s *Store) AssertSchemaVersion(ctx context.Context) error {
	v, err := s.SchemaVersion(ctx)
	if err != nil {
		return err
	}
	if v != ExpectedSchemaVersion {
		return fmt.Errorf(
			"schema version mismatch: database is at %d, this build expects %d; "+
				"run horopter-migrate at the matching version before starting this daemon",
			v, ExpectedSchemaVersion)
	}
	return nil
}

// OpenNoMigrate connects without applying migrations, for daemons that must not
// migrate. Only horopter-migrate changes the schema.
func OpenNoMigrate(ctx context.Context, dsn string) (*Store, error) {
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		return nil, fmt.Errorf("connect to postgres: %w; check DATABASE_URL", err)
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("ping postgres: %w; is the database reachable?", err)
	}
	return &Store{pool: pool}, nil
}
