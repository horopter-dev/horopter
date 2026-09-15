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
	"time"

	"github.com/yo61/horopter/internal/store/postgres"
	"github.com/yo61/horopter/internal/telemetry"
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

func run(ctx context.Context, dsn, rebuildTenant string) error {
	if dsn == "" {
		return errors.New("DATABASE_URL is required, e.g. " +
			"postgres://user:pass@host:5432/horopter")
	}

	shutdownTelemetry, err := telemetry.Setup(ctx, "horopter-migrate")
	if err != nil {
		return err
	}
	defer flushTelemetry(shutdownTelemetry)

	store, err := postgres.Open(ctx, dsn) // Open applies migrations under an advisory lock
	if err != nil {
		return err
	}
	defer store.Close()

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

// flushTelemetry drains buffered spans during shutdown.
//
// horopter-migrate has no signal handler and nothing else that cancels a context —
// main passes context.Background() straight through to run — so there is no
// cancelled context here to avoid reusing. A fresh context with its own timeout is
// still the right defensive default: what actually goes wrong during a flush is a
// collector that has become unreachable, and the timeout bounds exactly that.
func flushTelemetry(shutdown func(context.Context) error) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := shutdown(ctx); err != nil {
		slog.Warn("flushing telemetry failed; some spans were lost", "error", err)
	}
}
