package postgres

import (
	"context"
	"fmt"
)

// ExpectedSchemaVersion is the schema this build understands. Ingress and egress ship
// as separate images and can therefore be deployed at different versions; each asserts
// this at startup so a skewed rollout fails loudly instead of corrupting data quietly.
//
// Migrate derives the recorded version from the highest filename in the applied_migration
// ledger — what the DATABASE has actually run, not what this binary embeds — so this
// constant must be raised in the same change that adds a migration. It is deliberately not
// read from the SQL: a daemon has to be able to refuse a schema newer than the one its
// binary was built against, and an old binary run against a newer database records that
// database's true, higher version precisely so this assertion fails loudly.
const ExpectedSchemaVersion = 8

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
func OpenNoMigrate(ctx context.Context, dsn string, opts ...Option) (*Store, error) {
	pool, err := connect(ctx, dsn, "check DATABASE_URL")
	if err != nil {
		return nil, err
	}
	return storeFromPool(pool, opts), nil
}
