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

// bootstrapBinding is everything bootstrap owns about one credential: what the daemons
// authenticate it as, and the secret they authenticate it by. Revocation is deliberately not
// in it — see ensureCredential.
type bootstrapBinding struct {
	tenantID, role, source, locus, hash string
}

// storedCredential is an existing credential row as ensureCredential reads it.
type storedCredential struct {
	binding bootstrapBinding
	revoked bool
}

// ensureCredential makes the credential behind explicitKey exist, bound to this bootstrap's
// tenant, role, source and locus, and carrying explicitKey's own secret.
//
// It RECONCILES an existing row rather than skipping it. "A credential with this prefix
// exists" is not the fact bootstrap needs — "the stack authenticates as this tenant with this
// key" is — and the two come apart whenever a development database carries residue: a key
// bound to an earlier tenant or role stays bound to it, both daemons authenticate against the
// stale row, and bootstrap reports success. Because bootstrap runs on every container start,
// skipping makes that permanent rather than transient.
//
// The one mismatch it will not reconcile is a revoked row. Revocation is an operator's
// deliberate act and bootstrap does not own it; clearing it on the next container start would
// undo that decision with nothing said. That fails loudly instead, naming the prefix.
func ensureCredential(
	ctx context.Context, pool *pgxpool.Pool, bctx bootstrapContext, role, explicitKey string,
) error {
	prefix, secret, err := parseExplicitKey(explicitKey)
	if err != nil {
		return err
	}
	want := bootstrapBinding{
		tenantID: bctx.tenantID, role: role, source: bctx.source, locus: bctx.locus,
		hash: auth.HashSecret(secret),
	}

	got, found, err := loadCredential(ctx, pool, prefix)
	if err != nil {
		return err
	}
	if !found {
		fmt.Fprintf(os.Stderr,
			"warning: %s credential uses an explicit key; development only — never use it "+
				"in production\n", role)
		return issueCredential(ctx, pool, issueParams{
			tenant: want.tenantID, role: want.role, source: want.source, locus: want.locus,
			prefix: prefix, hash: want.hash,
		})
	}
	if got.revoked {
		return fmt.Errorf(
			"credential %s is revoked; bootstrap will not un-revoke one — issue a fresh key, "+
				"or clear revoked_at deliberately if the revocation was a mistake", prefix)
	}
	return reconcileCredential(ctx, pool, prefix, got.binding, want)
}

// loadCredential reads the credential stored under prefix, reporting false when there is none.
func loadCredential(
	ctx context.Context, pool *pgxpool.Pool, prefix string,
) (storedCredential, bool, error) {
	var c storedCredential
	err := pool.QueryRow(ctx, `
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

// reconcileCredential rewrites the stored row to want when it differs, naming on stderr which
// fields moved so the correction is visible rather than silent.
func reconcileCredential(
	ctx context.Context, pool *pgxpool.Pool, prefix string, got, want bootstrapBinding,
) error {
	if got == want {
		return nil
	}
	_, err := pool.Exec(ctx, `
		UPDATE credential SET tenant_id = $1, role = $2, source = $3, locus = $4, key_hash = $5
		WHERE key_prefix = $6`,
		want.tenantID, want.role, want.source, want.locus, want.hash, prefix)
	if err != nil {
		return fmt.Errorf("reconcile credential %s: %w", prefix, err)
	}
	fmt.Fprintf(os.Stderr, "reconciled credential %s: %s\n", prefix, describeDrift(got, want))
	return nil
}

// describeDrift names the fields that differ. The secret is reported as replaced and never
// printed, so a reconciliation line is safe in a container log.
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
