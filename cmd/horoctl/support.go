package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"flag"
	"fmt"
	"os"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
	"horopter.dev/horopter/internal/auth"
)

// dbExecer is the Exec/QueryRow subset both *pgxpool.Pool and pgx.Tx provide, mirroring
// internal/store/postgres's own querier interface. It lets issueCredential and bootstrap's
// credential helpers run unchanged whether called directly against the pool, as
// runKeysIssue does, or inside bootstrap's single locked transaction — see resolveTenant.
type dbExecer interface {
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

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
//
// It takes issueFlags rather than two strings because the rule is asymmetric — swap the
// arguments and an absent SOURCE would take the locus instead, which is a different rule
// that used to compile.
func resolveLocus(f issueFlags) string {
	if f.locus == "" {
		return f.source
	}
	return f.locus
}

// tenantLabel renders an empty tenant id (a global credential) readably in operator
// output, where an empty string would otherwise look like a blank field.
func tenantLabel(tenant string) string {
	if tenant == "" {
		return "global"
	}
	return tenant
}

// roleIsAppendOnly reads role's stored capabilities and reports whether they are append and
// nothing else — the only shape a subject pattern narrows anything real about. A read is not
// narrowed by subject at all, and a rebuild an admin grant triggers is whole-tenant by
// construction, so both are refused a pattern exactly as append-only roles are allowed one.
//
// It queries the row rather than enumerating role names, so issue time and request time agree
// by construction: internal/auth's principal() refuses an admin-capable credential carrying a
// subject pattern by testing the same capability list. Enumerating names instead let a role
// added to the table — one granting read or admin under a name nothing here knew — be issued
// with a subject pattern that either 401s on every request (admin) or is silently ignored on
// every read (Finding 1), with a key already printed either way.
func roleIsAppendOnly(ctx context.Context, pool *pgxpool.Pool, role string) (bool, error) {
	var appendOnly bool
	err := pool.QueryRow(ctx, `SELECT capabilities = ARRAY[$1]::text[] FROM role WHERE name = $2`,
		string(auth.CapAppend), role).Scan(&appendOnly)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, fmt.Errorf(
			"unknown role %q; the role table is what names the roles this database has", role)
	}
	if err != nil {
		return false, fmt.Errorf("read the capabilities of role %q: %w", role, err)
	}
	return appendOnly, nil
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
