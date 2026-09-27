// Package migrate applies the database schema and rebuilds a tenant's projection from its
// event log. It backs both cmd/horopter-migrate's thin main and horoctl's compiled-in
// migrate verb, so the schema-changing logic exists exactly once regardless of which
// binary an operator runs — and both entry points are compiled in, never resolved through
// horoctl's PATH-dispatched plugin fallthrough, so a differently-versioned plugin can
// never apply DDL.
//
// A rebuild folds the event log through postgres.Store.RebuildProjection, which uses
// core.Fold, and so must only ever run against the schema this build understands — the
// same condition Run's own version check asserts.
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

// serviceName is the telemetry service name for every caller of Run, deliberately the
// same regardless of whether the operator typed horopter-migrate or horoctl migrate: both
// are the same build running the same migration, so a trace or metric from either should
// resolve to the same service.
const serviceName = "horopter-migrate"

// RebuildHelp is the flag help text every caller of Run's rebuildTenant parameter should
// surface, stated up front because there is no way to discover the operational cost from
// a progress report: there isn't one.
const RebuildHelp = "tenant whose projection to discard and rebuild from its event log, " +
	"after migrating. Every append for that tenant blocks for the whole rebuild and then " +
	"fails retryably; the rebuild is not batched, reports no progress, and cannot run online"

// Run applies every pending migration against dsn and, when rebuildTenant is not empty,
// rebuilds that tenant's projection from its event log afterward.
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
	// Flush builds a fresh context because a daemon's signal handler has already
	// cancelled the one it was serving on. Run installs no signal handler — its caller
	// passes context.Background() straight through — so there is no cancelled context to
	// avoid here; what the fresh context and its timeout still buy is a bound on a
	// collector that has become unreachable. Run serves no /metrics: it returns before
	// anything could scrape it.
	defer tel.Flush()

	store, err := postgres.Open(ctx, dsn) // Open applies migrations under an advisory lock
	if err != nil {
		return err
	}
	defer store.Close()

	return verifyAndRebuild(ctx, store, rebuildTenant)
}

// logAppliedMigrations states what this run changed. Without it the only line a migrate
// Job emitted was "schema up to date", which a run that built the whole schema and a run
// that found it already current both produced identically — leaving an operator no way to
// tell from the logs whether any DDL had touched their database.
//
// One line naming every file, rather than a line per file, because they are applied in a
// single transaction: there is no moment at which some are applied and the rest are not,
// and a line each would imply a sequence that could be caught halfway through.
func logAppliedMigrations(applied []string) {
	if len(applied) == 0 {
		slog.Info("no migrations to apply; the schema was already current")
		return
	}
	slog.Info("applied migrations", "files", applied, "count", len(applied))
}

func verifyAndRebuild(ctx context.Context, store *postgres.Store, rebuildTenant string) error {
	// Before the version assertion, so a run that applied migrations and then failed that
	// assertion still reports the DDL it committed.
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
	// The rebuild runs only once the version assertion has passed. Replaying the log
	// through this build's fold into a schema it does not understand would write rows
	// shaped for the wrong schema over a projection that was correct for the right one.
	slog.Info("rebuilding projection", "tenant", rebuildTenant)
	if err := store.RebuildProjection(ctx, rebuildTenant); err != nil {
		return fmt.Errorf("rebuild the projection for tenant %q: %w", rebuildTenant, err)
	}
	slog.Info("projection rebuilt from the event log", "tenant", rebuildTenant)
	return nil
}
