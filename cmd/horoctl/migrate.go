package main

import (
	"context"
	"errors"
	"flag"
	"os"

	"horopter.dev/horopter/internal/migrate"
)

// runMigrate shares internal/migrate.Run with cmd/horopter-migrate.
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
