// Command horopter-migrate applies the database schema. It is the only binary that
// changes the schema; run it as a Job or init container before the daemons start.
//
// It also carries the operator surface for rebuilding a tenant's projection, because a
// rebuild folds the event log through this binary's core.Fold and so must only ever run
// against the schema this binary understands — the same condition the migration asserts.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"os"

	"github.com/horopter-dev/horopter/internal/logging"
	"github.com/horopter-dev/horopter/internal/store/postgres"
	"github.com/horopter-dev/horopter/internal/telemetry"
)

// rebuildHelp is the flag's help text. It states the operational cost up front because
// there is no way to discover it from a progress report: there isn't one.
const rebuildHelp = "tenant whose projection to discard and rebuild from its event log, " +
	"after migrating. Every append for that tenant blocks for the whole rebuild and then " +
	"fails retryably; the rebuild is not batched, reports no progress, and cannot run online"

func main() {
	dsn := flag.String("dsn", os.Getenv("DATABASE_URL"), "PostgreSQL connection string")
	rebuildTenant := flag.String("rebuild-tenant", "", rebuildHelp)
	flag.Parse()

	if err := run(context.Background(), *dsn, *rebuildTenant); err != nil {
		slog.Error("horopter-migrate failed", "error", err)
		os.Exit(1)
	}
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
				"migrated by a different build of horopter-migrate",
			v, postgres.ExpectedSchemaVersion)
	}
	slog.Info("schema up to date", "version", v)

	if rebuildTenant == "" {
		return nil
	}
	// The rebuild runs only once the version assertion has passed. Replaying the log
	// through this binary's fold into a schema it does not understand would write rows
	// shaped for the wrong schema over a projection that was correct for the right one.
	slog.Info("rebuilding projection", "tenant", rebuildTenant)
	if err := store.RebuildProjection(ctx, rebuildTenant); err != nil {
		return fmt.Errorf("rebuild the projection for tenant %q: %w", rebuildTenant, err)
	}
	slog.Info("projection rebuilt from the event log", "tenant", rebuildTenant)
	return nil
}

func run(ctx context.Context, dsn, rebuildTenant string) error {
	if err := logging.Setup(); err != nil {
		return err
	}

	if dsn == "" {
		return errors.New("DATABASE_URL is required, e.g. " +
			"postgres://user:pass@host:5432/horopter")
	}

	tel, err := telemetry.Setup(ctx, "horopter-migrate")
	if err != nil {
		return err
	}
	// Flush builds a fresh context because a daemon's signal handler has already
	// cancelled the one it was serving on. horopter-migrate has no signal handler —
	// main passes context.Background() straight through — so there is no cancelled
	// context to avoid here; what the fresh context and its timeout still buy is a
	// bound on a collector that has become unreachable. horopter-migrate serves no
	// /metrics: it exits before anything could scrape it.
	defer tel.Flush()

	store, err := postgres.Open(ctx, dsn) // Open applies migrations under an advisory lock
	if err != nil {
		return err
	}
	defer store.Close()

	return verifyAndRebuild(ctx, store, rebuildTenant)
}
