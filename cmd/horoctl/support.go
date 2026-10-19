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

// dbExecer is the subset of *pgxpool.Pool and pgx.Tx that the credential writes use.
type dbExecer interface {
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

func dsnFlag(fs *flag.FlagSet) *string {
	return fs.String("dsn", os.Getenv("DATABASE_URL"), "PostgreSQL connection string")
}

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

// helpOrErr treats -h as success: the FlagSet has already printed usage.
func helpOrErr(err error) error {
	if errors.Is(err, flag.ErrHelp) {
		return nil
	}
	return err
}

// resolveLocus defaults the locus to the source.
func resolveLocus(f issueFlags) string {
	if f.locus == "" {
		return f.source
	}
	return f.locus
}

// tenantLabel names a global credential's empty tenant in operator output.
func tenantLabel(tenant string) string {
	if tenant == "" {
		return "global"
	}
	return tenant
}

// roleIsAppendOnly reads capabilities, not names, so it agrees with internal/auth's principal().
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

func newTenantID() (string, error) {
	raw := make([]byte, 16)
	if _, err := rand.Read(raw); err != nil {
		return "", fmt.Errorf("generate tenant id: %w", err)
	}
	return "tnt_" + hex.EncodeToString(raw), nil
}
