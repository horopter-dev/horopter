package main

import (
	"context"
	"crypto/rand"
	"errors"
	"flag"
	"fmt"
	"os"

	"github.com/jackc/pgx/v5/pgxpool"
	"horopter.dev/horopter/internal/auth"
)

// runAdminKeys dispatches admin keys' sub-verbs.
func runAdminKeys(args []string) error {
	if len(args) < 1 {
		return errors.New("usage: horoctl admin keys <issue|list|revoke>")
	}
	switch args[0] {
	case "issue":
		return runKeysIssue(args[1:])
	case "list":
		return runKeysList(args[1:])
	case "revoke":
		return runKeysRevoke(args[1:])
	default:
		return fmt.Errorf("unknown admin keys command %q", args[0])
	}
}

// issueParams is what issueCredential needs to write one credential row, grouped into a
// struct to stay within the five-positional-parameter limit.
type issueParams struct {
	tenant, role, source, locus, subjectPattern string
	prefix, hash                                string
}

// runKeysIssue validates the locus itself, so an operator gets ValidateLocus's readable
// error rather than the credential_locus_shape constraint violation the insert below
// would otherwise surface — the two enforcement points are deliberate, and both are
// tested.
func runKeysIssue(args []string) error {
	fs := flag.NewFlagSet("admin keys issue", flag.ContinueOnError)
	dsn := dsnFlag(fs)
	tenant := fs.String("tenant", "",
		"opaque tenant id this credential is scoped to (omit for a global credential)")
	role := fs.String("role", "",
		"role name as the role table stores it; producer, reader, tenant_admin and "+
			"superuser are seeded")
	source := fs.String("source", "", `credential source, e.g. "github"`)
	locus := fs.String("locus", "", "ref namespace prefix; defaults to --source")
	subjectPattern := fs.String("subject-pattern", "",
		"restrict this credential to subjects matching this pattern")
	key := fs.String("key", "", explicitKeyHelp)
	if err := fs.Parse(args); err != nil {
		return helpOrErr(err)
	}

	effectiveLocus, err := resolveAndValidateIssue(*role, *source, *locus)
	if err != nil {
		return err
	}

	ctx := context.Background()
	pool, err := openPool(ctx, *dsn)
	if err != nil {
		return err
	}
	defer pool.Close()

	if err := checkRoleAllowsSubjectPattern(ctx, pool, *role, *subjectPattern); err != nil {
		return err
	}

	fullKey, prefix, hash, err := deriveKey(*key)
	if err != nil {
		return err
	}

	if err := issueCredential(ctx, pool, issueParams{
		tenant: *tenant, role: *role, source: *source, locus: effectiveLocus,
		subjectPattern: *subjectPattern, prefix: prefix, hash: hash,
	}); err != nil {
		return err
	}

	fmt.Fprintf(os.Stderr, "issued credential prefix=%s role=%s tenant=%s locus=%s\n",
		prefix, *role, tenantLabel(*tenant), effectiveLocus)
	// The key is printed exactly once, here, on stdout and nowhere else: it is
	// unrecoverable after this line, so KEY=$(horoctl admin keys issue ...) must be the
	// only place it appears.
	fmt.Println(fullKey)
	return nil
}

func validateIssueFlags(role, source string) error {
	if role == "" {
		return errors.New("--role is required")
	}
	if source == "" {
		return errors.New("--source is required")
	}
	return nil
}

// resolveAndValidateIssue runs the issue-time checks that need no database: the required
// flags and the locus, for which ValidateLocus gives a readable error rather than the
// credential_locus_shape constraint violation the insert would surface. Split out of
// runKeysIssue to keep that function's own branching within the complexity limit.
func resolveAndValidateIssue(role, source, locus string) (string, error) {
	if err := validateIssueFlags(role, source); err != nil {
		return "", err
	}
	effectiveLocus := resolveLocus(source, locus)
	if err := auth.ValidateLocus(effectiveLocus); err != nil {
		return "", err
	}
	return effectiveLocus, nil
}

// checkRoleAllowsSubjectPattern reads the role's stored capabilities and refuses a subject
// pattern on one granting admin — Constraint 5: a rebuild is whole-tenant by construction, so
// an admin grant cannot be narrowed by subject. The role is read whatever the pattern, so an
// unknown role also fails here rather than as a foreign-key violation after a key is minted.
func checkRoleAllowsSubjectPattern(
	ctx context.Context, pool *pgxpool.Pool, role, subjectPattern string,
) error {
	grants, err := roleGrantsAdmin(ctx, pool, role)
	if err != nil {
		return err
	}
	if grants && subjectPattern != "" {
		return fmt.Errorf(
			"role %q grants admin, and an admin grant cannot be narrowed by a subject "+
				"pattern", role)
	}
	return nil
}

// deriveKey returns the full key to print, its lookup prefix and its stored hash. An
// explicit value takes the --key development path, with its own warning; otherwise a
// fresh key is generated.
func deriveKey(explicit string) (key, prefix, hash string, err error) {
	if explicit == "" {
		return auth.NewKey(rand.Reader)
	}
	prefix, secret, err := parseExplicitKey(explicit)
	if err != nil {
		return "", "", "", err
	}
	fmt.Fprintln(os.Stderr,
		"warning: --key sets an explicit credential value; this is development only — "+
			"never use it in production")
	return explicit, prefix, auth.HashSecret(secret), nil
}

// issueCredential writes the principal and credential rows for one issued key. A
// principal is created per credential rather than reused: horoctl has no way to name an
// existing one, and decision 4's several-credentials-per-principal model is served just as
// well by principals that start with one credential each as it would be by a shared one.
func issueCredential(ctx context.Context, pool *pgxpool.Pool, p issueParams) error {
	principalID := "prn_" + p.prefix
	credentialID := "cred_" + p.prefix
	var tenantID, pattern *string
	if p.tenant != "" {
		tenantID = &p.tenant
	}
	if p.subjectPattern != "" {
		pattern = &p.subjectPattern
	}

	if _, err := pool.Exec(ctx,
		`INSERT INTO principal (id, display_name) VALUES ($1, $1)`, principalID); err != nil {
		return fmt.Errorf("create principal for credential: %w", err)
	}
	_, err := pool.Exec(ctx, `
		INSERT INTO credential
			(id, principal_id, type, key_prefix, key_hash, role, tenant_id, source, locus,
			 subject_pattern)
		VALUES ($1, $2, 'machine_key', $3, $4, $5, $6, $7, $8, $9)`,
		credentialID, principalID, p.prefix, p.hash, p.role, tenantID, p.source, p.locus,
		pattern)
	if err != nil {
		return fmt.Errorf("issue credential: %w", err)
	}
	return nil
}

func runKeysList(args []string) error {
	fs := flag.NewFlagSet("admin keys list", flag.ContinueOnError)
	dsn := dsnFlag(fs)
	tenant := fs.String("tenant", "", "only list credentials scoped to this tenant id")
	if err := fs.Parse(args); err != nil {
		return helpOrErr(err)
	}

	ctx := context.Background()
	pool, err := openPool(ctx, *dsn)
	if err != nil {
		return err
	}
	defer pool.Close()

	return listCredentials(ctx, pool, *tenant)
}

func listCredentials(ctx context.Context, pool *pgxpool.Pool, tenant string) error {
	query := `SELECT key_prefix, role, COALESCE(tenant_id, ''), locus, source,
	                  revoked_at IS NOT NULL
	           FROM credential`
	var args []any
	if tenant != "" {
		query += ` WHERE tenant_id = $1`
		args = append(args, tenant)
	}
	query += ` ORDER BY created_at`

	rows, err := pool.Query(ctx, query, args...)
	if err != nil {
		return fmt.Errorf("list credentials: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var prefix, role, tenantID, locus, source string
		var revoked bool
		if err := rows.Scan(&prefix, &role, &tenantID, &locus, &source, &revoked); err != nil {
			return err
		}
		fmt.Printf("%s\trole=%s\ttenant=%s\tlocus=%s\tsource=%s\trevoked=%t\n",
			prefix, role, tenantLabel(tenantID), locus, source, revoked)
	}
	return rows.Err()
}

func runKeysRevoke(args []string) error {
	fs := flag.NewFlagSet("admin keys revoke", flag.ContinueOnError)
	dsn := dsnFlag(fs)
	prefix := fs.String("prefix", "", "key prefix of the credential to revoke")
	if err := fs.Parse(args); err != nil {
		return helpOrErr(err)
	}
	if *prefix == "" {
		return errors.New("--prefix is required")
	}

	ctx := context.Background()
	pool, err := openPool(ctx, *dsn)
	if err != nil {
		return err
	}
	defer pool.Close()

	tag, err := pool.Exec(ctx,
		`UPDATE credential SET revoked_at = now() WHERE key_prefix = $1 AND revoked_at IS NULL`,
		*prefix)
	if err != nil {
		return fmt.Errorf("revoke credential %q: %w", *prefix, err)
	}
	if tag.RowsAffected() == 0 {
		return fmt.Errorf("no active credential with prefix %q", *prefix)
	}
	fmt.Fprintf(os.Stderr, "revoked credential with prefix %s\n", *prefix)
	return nil
}
