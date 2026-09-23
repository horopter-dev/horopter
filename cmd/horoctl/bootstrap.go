package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"horopter.dev/horopter/internal/auth"
)

// bootstrapContext carries the fields every credential bootstrap issues shares, keeping
// ensureCredential within the five-positional-parameter limit.
type bootstrapContext struct {
	tenantID, source, locus string
}

// bootstrapRequest carries the operator's flags into bootstrapTenantAndKeys as one value,
// keeping it within the five-positional-parameter limit.
type bootstrapRequest struct {
	displayName, source, locus, producerKey, readerKey string
}

// runAdminBootstrap is the dev-stack convenience verb: it creates the tenant if absent and
// issues a producer and a reader credential if their prefixes are absent. It is one verb
// rather than two chained commands because the compose image is distroless/static and has
// no shell to chain with, and because a fresh database has no tenant for `keys issue` to
// target — 0004's backfill derives tenants from existing rows and finds none.
//
// It never issues a superuser. Only producer and reader are reachable here, both fixed:
// of the four seeded roles only superuser grants both append and read, and it also grants
// issue, which would put a non-expiring credential able to mint other credentials into
// .env — exactly what a management-plane credential must never be.
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
	effectiveLocus := resolveLocus(*source, *locus)
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

// bootstrapTenantAndKeys resolves the tenant and ensures both credentials exist, reporting
// completion once both are in place. Grouped into its own function, past flag parsing, so
// runAdminBootstrap stays a single readable sequence of validate-then-connect-then-act.
func bootstrapTenantAndKeys(ctx context.Context, pool *pgxpool.Pool, req bootstrapRequest) error {
	tenantID, err := resolveTenant(ctx, pool, req.displayName)
	if err != nil {
		return err
	}
	bctx := bootstrapContext{tenantID: tenantID, source: req.source, locus: req.locus}

	for _, c := range []struct{ role, key string }{
		{"producer", req.producerKey},
		{"reader", req.readerKey},
	} {
		if err := ensureCredential(ctx, pool, bctx, c.role, c.key); err != nil {
			return fmt.Errorf("bootstrap %s credential: %w", c.role, err)
		}
	}
	fmt.Fprintf(os.Stderr, "bootstrap complete: tenant %s (%q)\n", tenantID, req.displayName)
	return nil
}

// resolveTenant finds the tenant already bootstrapped under displayName, or creates one.
// Matching by display name is bootstrap's own convenience for a fixed, well-known dev
// tenant; it is not the general resolution mechanism keys issue's --tenant deliberately
// refuses to offer, because display names are mutable and not unique by design.
func resolveTenant(ctx context.Context, pool *pgxpool.Pool, displayName string) (string, error) {
	var id string
	err := pool.QueryRow(ctx,
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
	if _, err := pool.Exec(ctx,
		`INSERT INTO tenant (id, display_name) VALUES ($1, $2)`, id, displayName); err != nil {
		return "", fmt.Errorf("create tenant %q: %w", displayName, err)
	}
	return id, nil
}

// ensureCredential issues role's credential under explicitKey's prefix unless a credential
// with that prefix already exists, which is what makes bootstrap idempotent: `keys issue`
// always mints a fresh credential, but bootstrap's whole purpose is being safe to run on
// every container start.
func ensureCredential(
	ctx context.Context, pool *pgxpool.Pool, bctx bootstrapContext, role, explicitKey string,
) error {
	prefix, secret, err := parseExplicitKey(explicitKey)
	if err != nil {
		return err
	}

	var exists bool
	err = pool.QueryRow(ctx,
		`SELECT EXISTS (SELECT 1 FROM credential WHERE key_prefix = $1)`, prefix).Scan(&exists)
	if err != nil {
		return fmt.Errorf("check existing credential: %w", err)
	}
	if exists {
		return nil
	}

	fmt.Fprintf(os.Stderr,
		"warning: %s credential uses an explicit key; development only — never use it in "+
			"production\n", role)
	return issueCredential(ctx, pool, issueParams{
		tenant: bctx.tenantID, role: role, source: bctx.source, locus: bctx.locus,
		prefix: prefix, hash: auth.HashSecret(secret),
	})
}
