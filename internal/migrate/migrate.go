// Package migrate applies the schema and rebuilds a tenant's projection, for both migrate binaries.
package migrate

import (
	"context"
	"errors"
	"fmt"
	"log/slog"

	"horopter.dev/horopter/internal/logging"
	"horopter.dev/horopter/internal/store/postgres"
	"horopter.dev/horopter/internal/telemetry"
)

// serviceName is the same whichever binary ran: both are the same migration.
const serviceName = "horopter-migrate"

// RebuildHelp is the flag help for rebuildTenant: a rebuild reports no progress.
const RebuildHelp = "tenant whose projection to discard and rebuild from its event log, " +
	"after migrating. Every append for that tenant blocks for the whole rebuild and then " +
	"fails retryably; the rebuild is not batched, reports no progress, and cannot run online"

// Run applies every pending migration against dsn, then rebuilds rebuildTenant if set.
func Run(ctx context.Context, dsn, rebuildTenant string) error {
	if err := logging.Setup(); err != nil {
		return err
	}

	if dsn == "" {
		return errors.New("DATABASE_URL is required, e.g. " +
			"postgres://user:pass@host:5432/horopter")
	}

	tel, err := telemetry.Setup(ctx, serviceName)
	if err != nil {
		return err
	}
	// The flush timeout bounds a collector that has become unreachable.
	defer tel.Flush()

	store, err := postgres.Open(ctx, dsn) // Open applies migrations under an advisory lock
	if err != nil {
		return err
	}
	defer store.Close()

	return verifyAndRebuild(ctx, store, rebuildTenant)
}

// logAppliedMigrations logs one line: the files are applied in a single transaction.
func logAppliedMigrations(applied []string) {
	if len(applied) == 0 {
		slog.Info("no migrations to apply; the schema was already current")
		return
	}
	slog.Info("applied migrations", "files", applied, "count", len(applied))
}

func verifyAndRebuild(ctx context.Context, store *postgres.Store, rebuildTenant string) error {
	// Before the version assertion, so a failed run still reports the DDL it committed.
	logAppliedMigrations(store.AppliedMigrations())

	v, err := store.SchemaVersion(ctx)
	if err != nil {
		return err
	}
	if v != postgres.ExpectedSchemaVersion {
		return fmt.Errorf(
			"schema version reached %d but this binary expects %d; the database has been "+
				"migrated by a different, incompatible build",
			v, postgres.ExpectedSchemaVersion)
	}
	slog.Info("schema up to date", "version", v)

	if rebuildTenant == "" {
		return nil
	}
	// Only after the version check: this build's fold must not write a schema it does not know.
	slog.Info("rebuilding projection", "tenant", rebuildTenant)
	if err := store.RebuildProjection(ctx, rebuildTenant); err != nil {
		return fmt.Errorf("rebuild the projection for tenant %q: %w", rebuildTenant, err)
	}
	slog.Info("projection rebuilt from the event log", "tenant", rebuildTenant)
	return nil
}
