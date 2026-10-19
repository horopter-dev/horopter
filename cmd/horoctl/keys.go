package main

import (
	"context"
	"crypto/rand"
	"errors"
	"flag"
	"fmt"
	"os"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"horopter.dev/horopter/internal/auth"
	"horopter.dev/horopter/internal/core"
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

type issueParams struct {
	tenant, role, source, locus, subjectPattern string
	prefix, hash                                string
}

// runKeysIssue mints a credential and prints its key once, on stdout.
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

	effectiveLocus, err := resolveAndValidateIssue(issueFlags{
		role: *role, source: *source, locus: *locus,
	})
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

	if err := issueCredentialTx(ctx, pool, issueParams{
		tenant: *tenant, role: *role, source: *source, locus: effectiveLocus.String(),
		subjectPattern: *subjectPattern, prefix: prefix, hash: hash,
	}); err != nil {
		return err
	}

	fmt.Fprintf(os.Stderr, "issued credential prefix=%s role=%s tenant=%s locus=%s\n",
		prefix, *role, tenantLabel(*tenant), effectiveLocus)
	// The only place the key appears: it cannot be recovered after this line.
	fmt.Println(fullKey)
	return nil
}

// issueFlags names role, source and locus so a call site cannot transpose them.
type issueFlags struct{ role, source, locus string }

func validateIssueFlags(f issueFlags) error {
	if f.role == "" {
		return errors.New("--role is required")
	}
	if f.source == "" {
		return errors.New("--source is required")
	}
	return nil
}

// resolveAndValidateIssue runs the issue-time checks that need no database.
func resolveAndValidateIssue(f issueFlags) (core.Locus, error) {
	if err := validateIssueFlags(f); err != nil {
		return core.Locus{}, err
	}
	effective := resolveLocus(f)
	if err := auth.ValidateLocus(effective); err != nil {
		return core.Locus{}, err
	}
	// Cannot fail: ValidateLocus enforces core.NewLocus's rules and more.
	locus, err := core.NewLocus(effective)
	if err != nil {
		return core.Locus{}, err
	}
	return locus, nil
}

// checkRoleAllowsSubjectPattern refuses a subject pattern on a role granting more than append.
func checkRoleAllowsSubjectPattern(
	ctx context.Context, pool *pgxpool.Pool, role, subjectPattern string,
) error {
	appendOnly, err := roleIsAppendOnly(ctx, pool, role)
	if err != nil {
		return err
	}
	if subjectPattern != "" && !appendOnly {
		return fmt.Errorf(
			"role %q grants more than append, and a subject pattern only narrows what a role "+
				"may append; reads and admin rebuilds are whole-tenant regardless of subject",
			role)
	}
	return nil
}

// deriveKey returns the key to print, its prefix and its hash, generating one if explicit is empty.
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

// issueCredentialTx issues in one transaction, so a bad --tenant leaves no orphaned principal.
func issueCredentialTx(ctx context.Context, pool *pgxpool.Pool, p issueParams) error {
	tx, err := pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin issue transaction: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	if p.tenant != "" {
		if err := checkTenantExists(ctx, tx, p.tenant); err != nil {
			return err
		}
	}
	if err := issueCredential(ctx, tx, p); err != nil {
		return err
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit issue transaction: %w", err)
	}
	return nil
}

func checkTenantExists(ctx context.Context, tx pgx.Tx, tenant string) error {
	var exists bool
	if err := tx.QueryRow(ctx,
		`SELECT EXISTS (SELECT 1 FROM tenant WHERE id = $1)`, tenant).Scan(&exists); err != nil {
		return fmt.Errorf("check tenant %q exists: %w", tenant, err)
	}
	if !exists {
		return fmt.Errorf("tenant %q does not exist", tenant)
	}
	return nil
}

// issueCredential writes one principal and one credential; the caller owns the transaction.
func issueCredential(ctx context.Context, db dbExecer, p issueParams) error {
	principalID := "prn_" + p.prefix
	credentialID := "cred_" + p.prefix
	var tenantID, pattern *string
	if p.tenant != "" {
		tenantID = &p.tenant
	}
	if p.subjectPattern != "" {
		pattern = &p.subjectPattern
	}

	if _, err := db.Exec(ctx,
		`INSERT INTO principal (id, display_name) VALUES ($1, $1)`, principalID); err != nil {
		return fmt.Errorf("create principal for credential: %w", err)
	}
	_, err := db.Exec(ctx, `
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
