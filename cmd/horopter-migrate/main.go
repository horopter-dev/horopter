// Command horopter-migrate applies the database schema; run it before the daemons start.
package main

import (
	"context"
	"flag"
	"log/slog"
	"os"

	"horopter.dev/horopter/internal/buildinfo"
	"horopter.dev/horopter/internal/migrate"
)

func main() {
	code, handled := buildinfo.Handle("horopter-migrate", os.Args[1:], os.Stdout, os.Stderr)
	if handled {
		os.Exit(code)
	}
	dsn := flag.String("dsn", os.Getenv("DATABASE_URL"), "PostgreSQL connection string")
	rebuildTenant := flag.String("rebuild-tenant", "", migrate.RebuildHelp)
	flag.Parse()

	if err := migrate.Run(context.Background(), *dsn, *rebuildTenant); err != nil {
		slog.Error("horopter-migrate failed", "error", err)
		os.Exit(1)
	}
}
