package main

import (
	"context"
	"errors"
	"flag"
	"os"

	"horopter.dev/horopter/internal/migrate"
)

// runMigrate is horoctl's compiled-in migrate verb. It calls the same internal/migrate.Run
// entry point cmd/horoctl-migrate's thin main does, so the two never disagree on what a
// migration does — see main.go's package comment for why this verb is never delegated.
func runMigrate(args []string) error {
	fs := flag.NewFlagSet("migrate", flag.ContinueOnError)
	dsn := fs.String("dsn", os.Getenv("DATABASE_URL"), "PostgreSQL connection string")
	rebuildTenant := fs.String("rebuild-tenant", "", migrate.RebuildHelp)
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		return err
	}
	return migrate.Run(context.Background(), *dsn, *rebuildTenant)
}
