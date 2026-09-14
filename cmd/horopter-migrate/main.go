// Command horopter-migrate applies the database schema. It is the only binary that
// changes the schema; run it as a Job or init container before the daemons start.
package main

import (
	"context"
	"flag"
	"log/slog"
	"os"

	"github.com/yo61/horopter/internal/store/postgres"
)

func main() {
	dsn := flag.String("dsn", os.Getenv("DATABASE_URL"), "PostgreSQL connection string")
	flag.Parse()

	if *dsn == "" {
		slog.Error("DATABASE_URL is required, e.g. postgres://user:pass@host:5432/horopter")
		os.Exit(1)
	}
	ctx := context.Background()
	store, err := postgres.Open(ctx, *dsn) // Open applies migrations under an advisory lock
	if err != nil {
		slog.Error("migration failed", "error", err)
		os.Exit(1)
	}
	defer store.Close()

	v, err := store.SchemaVersion(ctx)
	if err != nil {
		slog.Error("could not confirm schema version", "error", err)
		os.Exit(1)
	}
	if v != postgres.ExpectedSchemaVersion {
		slog.Error("schema version did not reach the expected value",
			"got", v, "want", postgres.ExpectedSchemaVersion)
		os.Exit(1)
	}
	slog.Info("schema up to date", "version", v)
}
