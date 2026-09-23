package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"flag"
	"fmt"
	"os"

	"github.com/jackc/pgx/v5/pgxpool"
)

// dsnFlag registers the --dsn flag every DB-touching admin subcommand exposes, defaulting
// to DATABASE_URL so scripts need not repeat it on every invocation.
func dsnFlag(fs *flag.FlagSet) *string {
	return fs.String("dsn", os.Getenv("DATABASE_URL"), "PostgreSQL connection string")
}

// openPool connects to dsn, refusing an empty one before pgxpool produces a less readable
// error trying to parse it.
func openPool(ctx context.Context, dsn string) (*pgxpool.Pool, error) {
	if dsn == "" {
		return nil, errors.New(
			"DATABASE_URL is required, e.g. postgres://user:pass@host:5432/horopter")
	}
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		return nil, fmt.Errorf("connect to the database: %w", err)
	}
	return pool, nil
}

// helpOrErr turns flag.ErrHelp into a clean exit: -h/--help already printed usage to the
// FlagSet's own output, and the process should exit 0 rather than report a help request
// as a failure.
func helpOrErr(err error) error {
	if errors.Is(err, flag.ErrHelp) {
		return nil
	}
	return err
}

// resolveLocus applies the design's default: locus is admin-chosen and required, and
// defaults to source when the operator does not name one separately.
func resolveLocus(source, locus string) string {
	if locus == "" {
		return source
	}
	return locus
}

// tenantLabel renders an empty tenant id (a global credential) readably in operator
// output, where an empty string would otherwise look like a blank field.
func tenantLabel(tenant string) string {
	if tenant == "" {
		return "global"
	}
	return tenant
}

// isAdminRole matches migration 0004's credential_admin_has_no_subject_pattern CHECK, so
// keys issue can refuse the combination with a readable error at issue time rather than a
// constraint violation. The middleware in internal/auth is the authority at request time.
func isAdminRole(role string) bool {
	return role == "tenant_admin" || role == "superuser"
}

// newTenantID mints an opaque, server-assigned tenant id. The display name is what an
// operator recognizes a tenant by; the id exists so a rename is a single-row update rather
// than a rewrite of every event and change row that names it.
func newTenantID() (string, error) {
	raw := make([]byte, 16)
	if _, err := rand.Read(raw); err != nil {
		return "", fmt.Errorf("generate tenant id: %w", err)
	}
	return "tnt_" + hex.EncodeToString(raw), nil
}
