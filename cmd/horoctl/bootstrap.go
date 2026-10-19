package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"horopter.dev/horopter/internal/auth"
)

type bootstrapContext struct {
	tenantID, source, locus string
}

type bootstrapRequest struct {
	displayName, source, locus, producerKey, readerKey string
}

// runAdminBootstrap ensures a dev tenant and its producer and reader credentials exist.
func runAdminBootstrap(args []string) error {
	fs := flag.NewFlagSet("admin bootstrap", flag.ContinueOnError)
	dsn := dsnFlag(fs)
	displayName := fs.String("display-name", "", "display name of the tenant to create if absent")
	source := fs.String("source", "", "credential source for both issued keys")
	locus := fs.String("locus", "",
		"ref namespace prefix for both issued keys; defaults to --source")
	producerKey := fs.String("producer-key", "", "producer "+explicitKeyHelp)
	readerKey := fs.String("reader-key", "", "reader "+explicitKeyHelp)
	if err := fs.Parse(args); err != nil {
		return helpOrErr(err)
	}
	if err := validateBootstrapFlags(*displayName, *producerKey, *readerKey); err != nil {
		return err
	}
	effectiveLocus := resolveLocus(issueFlags{source: *source, locus: *locus})
	if err := auth.ValidateLocus(effectiveLocus); err != nil {
		return err
	}

	ctx := context.Background()
	pool, err := openPool(ctx, *dsn)
	if err != nil {
		return err
	}
	defer pool.Close()

	return bootstrapTenantAndKeys(ctx, pool, bootstrapRequest{
		displayName: *displayName, source: *source, locus: effectiveLocus,
		producerKey: *producerKey, readerKey: *readerKey,
	})
}

func validateBootstrapFlags(displayName, producerKey, readerKey string) error {
	if displayName == "" {
		return errors.New("--display-name is required")
	}
	if producerKey == "" || readerKey == "" {
		return errors.New("--producer-key and --reader-key are required")
	}
	return nil
}

// bootstrapTenantAndKeys uses one transaction, so resolveTenant's lock covers both credentials.
func bootstrapTenantAndKeys(ctx context.Context, pool *pgxpool.Pool, req bootstrapRequest) error {
	tx, err := pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin bootstrap transaction: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	tenantID, err := resolveTenant(ctx, tx, req.displayName)
	if err != nil {
		return err
	}
	bctx := bootstrapContext{tenantID: tenantID, source: req.source, locus: req.locus}

	for _, c := range []struct{ role, key string }{
		{"producer", req.producerKey},
		{"reader", req.readerKey},
	} {
		if err := ensureCredential(ctx, tx, bctx, c.role, c.key); err != nil {
			return fmt.Errorf("bootstrap %s credential: %w", c.role, err)
		}
	}

	// Report only after the commit: a failed commit applied nothing.
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit bootstrap transaction: %w", err)
	}
	fmt.Fprintf(os.Stderr, "bootstrap complete: tenant %s (%q)\n", tenantID, req.displayName)
	return nil
}

// resolveTenant locks first because display_name is not UNIQUE and concurrent runs race.
func resolveTenant(ctx context.Context, tx pgx.Tx, displayName string) (string, error) {
	if _, err := tx.Exec(ctx,
		`SELECT pg_advisory_xact_lock(hashtext($1))`, displayName); err != nil {
		return "", fmt.Errorf("take bootstrap advisory lock: %w", err)
	}
	return findOrCreateTenant(ctx, tx, displayName)
}

func findOrCreateTenant(ctx context.Context, tx pgx.Tx, displayName string) (string, error) {
	var id string
	err := tx.QueryRow(ctx,
		`SELECT id FROM tenant WHERE display_name = $1 ORDER BY created_at LIMIT 1`,
		displayName).Scan(&id)
	if err == nil {
		return id, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return "", fmt.Errorf("look up tenant %q: %w", displayName, err)
	}

	id, err = newTenantID()
	if err != nil {
		return "", err
	}
	if _, err := tx.Exec(ctx,
		`INSERT INTO tenant (id, display_name) VALUES ($1, $2)`, id, displayName); err != nil {
		return "", fmt.Errorf("create tenant %q: %w", displayName, err)
	}
	return id, nil
}

// bootstrapBinding is what bootstrap owns of a credential; revocation is not part of it.
type bootstrapBinding struct {
	tenantID, role, source, locus, hash string
}

// storedCredential is an existing credential row as ensureCredential reads it.
type storedCredential struct {
	binding bootstrapBinding
	revoked bool
}

// ensureCredential reconciles an existing credential to this bootstrap but never un-revokes one.
func ensureCredential(
	ctx context.Context, db dbExecer, bctx bootstrapContext, role, explicitKey string,
) error {
	prefix, secret, err := parseExplicitKey(explicitKey)
	if err != nil {
		return err
	}
	want := bootstrapBinding{
		tenantID: bctx.tenantID, role: role, source: bctx.source, locus: bctx.locus,
		hash: auth.HashSecret(secret),
	}

	got, found, err := loadCredential(ctx, db, prefix)
	if err != nil {
		return err
	}
	if !found {
		fmt.Fprintf(os.Stderr,
			"warning: %s credential uses an explicit key; development only — never use it "+
				"in production\n", role)
		return issueCredential(ctx, db, issueParams{
			tenant: want.tenantID, role: want.role, source: want.source, locus: want.locus,
			prefix: prefix, hash: want.hash,
		})
	}
	if got.revoked {
		return fmt.Errorf(
			"credential %s is revoked; bootstrap will not un-revoke one — issue a fresh key, "+
				"or clear revoked_at deliberately if the revocation was a mistake", prefix)
	}
	return reconcileCredential(ctx, db, prefix, got.binding, want)
}

// loadCredential reads the credential stored under prefix, reporting false when there is none.
func loadCredential(
	ctx context.Context, db dbExecer, prefix string,
) (storedCredential, bool, error) {
	var c storedCredential
	err := db.QueryRow(ctx, `
		SELECT COALESCE(tenant_id, ''), role, source, locus, key_hash, revoked_at IS NOT NULL
		FROM credential WHERE key_prefix = $1`, prefix).Scan(
		&c.binding.tenantID, &c.binding.role, &c.binding.source, &c.binding.locus,
		&c.binding.hash, &c.revoked)
	if errors.Is(err, pgx.ErrNoRows) {
		return storedCredential{}, false, nil
	}
	if err != nil {
		return storedCredential{}, false, fmt.Errorf("read credential %s: %w", prefix, err)
	}
	return c, true, nil
}

// reconcileCredential rewrites the stored row to want, naming on stderr what moved.
func reconcileCredential(
	ctx context.Context, db dbExecer, prefix string, got, want bootstrapBinding,
) error {
	if got == want {
		return nil
	}
	_, err := db.Exec(ctx, `
		UPDATE credential SET tenant_id = $1, role = $2, source = $3, locus = $4, key_hash = $5
		WHERE key_prefix = $6`,
		want.tenantID, want.role, want.source, want.locus, want.hash, prefix)
	if err != nil {
		return fmt.Errorf("reconcile credential %s: %w", prefix, err)
	}
	fmt.Fprintf(os.Stderr, "reconciled credential %s: %s\n", prefix, describeDrift(got, want))
	return nil
}

// describeDrift never prints the secret, so its output is safe in a container log.
func describeDrift(got, want bootstrapBinding) string {
	var moved []string
	for _, f := range []struct{ name, from, to string }{
		{"tenant", got.tenantID, want.tenantID},
		{"role", got.role, want.role},
		{"source", got.source, want.source},
		{"locus", got.locus, want.locus},
	} {
		if f.from != f.to {
			moved = append(moved, fmt.Sprintf("%s %s -> %s", f.name, f.from, f.to))
		}
	}
	if got.hash != want.hash {
		moved = append(moved, "key secret replaced")
	}
	return strings.Join(moved, ", ")
}
