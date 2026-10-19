package postgres

import (
	"context"
	"fmt"
)

// ExpectedSchemaVersion is the schema this build understands; raise it with every migration.
const ExpectedSchemaVersion = 11

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

// OpenNoMigrate connects without migrating; only horopter-migrate changes the schema.
func OpenNoMigrate(ctx context.Context, dsn string, opts ...Option) (*Store, error) {
	pool, err := connect(ctx, dsn, "check DATABASE_URL")
	if err != nil {
		return nil, err
	}
	return storeFromPool(pool, opts), nil
}
