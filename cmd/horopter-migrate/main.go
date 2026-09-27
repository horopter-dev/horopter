// Command horopter-migrate applies the database schema. Together with horoctl's
// compiled-in migrate verb — which calls the identical internal/migrate.Run entry point
// this thin main does — it is the only way the schema changes; run this binary as a Job
// or init container before the daemons start.
//
// It also carries the operator surface for rebuilding a tenant's projection, because a
// rebuild folds the event log through internal/migrate's core.Fold and so must only ever
// run against the schema this binary understands — the same condition the migration
// asserts. This binary exists separately from horoctl only so the init container image
// stays minimal — see internal/migrate's package comment.
package main

import (
	"context"
	"flag"
	"log/slog"
	"os"

	"horopter.dev/horopter/internal/migrate"
)

func main() {
	dsn := flag.String("dsn", os.Getenv("DATABASE_URL"), "PostgreSQL connection string")
	rebuildTenant := flag.String("rebuild-tenant", "", migrate.RebuildHelp)
	flag.Parse()

	if err := migrate.Run(context.Background(), *dsn, *rebuildTenant); err != nil {
		slog.Error("horopter-migrate failed", "error", err)
		os.Exit(1)
	}
}
