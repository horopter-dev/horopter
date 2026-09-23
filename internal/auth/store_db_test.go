package auth_test

import (
	"context"
	"crypto/rand"
	"os"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"horopter.dev/horopter/internal/auth"
	"horopter.dev/horopter/internal/testdb"
)

// credentialSpec is one credential to seed. A global credential is one whose tenant_id
// is NULL, which is the distinction the Store exists to read correctly.
type credentialSpec struct {
	role           string
	global         bool
	subjectPattern *string
	expiresAt      *time.Time
}

// seeded is what a seeded credential's test needs back: the row's identifiers and the
// prefix a lookup presents.
type seeded struct {
	credentialID string
	principalID  string
	tenantID     string
	prefix       string
}

func credentialStore(t *testing.T) *auth.Store {
	t.Helper()
	if os.Getenv(testdb.DSNVar) == "" {
		t.Skip("set " + testdb.DSNVar + " to run Postgres tests (docker compose up -d db)")
	}
	pool, err := testdb.Pool()
	require.NoError(t, err)
	return auth.NewStore(pool)
}

func execDB(t *testing.T, sql string, args ...any) {
	t.Helper()
	pool, err := testdb.Pool()
	require.NoError(t, err)
	_, err = pool.Exec(context.Background(), sql, args...)
	require.NoError(t, err)
}

// seedCredential inserts a principal, a tenant unless the credential is global, and the
// credential itself, registering the cleanup that removes all three. Identifiers derive
// from the generated key prefix, so no two runs collide in the shared database.
func seedCredential(t *testing.T, spec credentialSpec) seeded {
	t.Helper()
	_, prefix, hash, err := auth.NewKey(rand.Reader)
	require.NoError(t, err)

	row := seeded{
		credentialID: "cred_test_" + prefix,
		principalID:  "prn_test_" + prefix,
		prefix:       prefix,
	}
	var tenant *string
	if !spec.global {
		row.tenantID = "tnt_test_" + prefix
		tenant = &row.tenantID
		execDB(t, `INSERT INTO tenant (id, display_name) VALUES ($1, $1)`, row.tenantID)
	}
	execDB(t, `INSERT INTO principal (id, display_name) VALUES ($1, $1)`, row.principalID)
	execDB(t, `
		INSERT INTO credential (id, principal_id, type, key_prefix, key_hash, role,
		                        tenant_id, source, locus, subject_pattern, expires_at)
		VALUES ($1, $2, 'machine_key', $3, $4, $5, $6, 'test', 'github', $7, $8)`,
		row.credentialID, row.principalID, prefix, hash, spec.role, tenant,
		spec.subjectPattern, spec.expiresAt)

	t.Cleanup(func() { dropSeeded(t, row) })
	return row
}

// dropSeeded removes the rows in foreign-key order. A failure is logged rather than
// failing the test, as testdb's own cleanups are: residue is what this removes, so
// failing an otherwise passing test over it would trade a tidy database for a flaky suite.
func dropSeeded(t *testing.T, row seeded) {
	pool, err := testdb.Pool()
	if err != nil {
		t.Logf("credential cleanup for %s: open pool: %v", row.credentialID, err)
		return
	}
	ctx := context.Background()
	for _, stmt := range []struct{ sql, arg string }{
		{`DELETE FROM credential WHERE id = $1`, row.credentialID},
		{`DELETE FROM principal WHERE id = $1`, row.principalID},
		{`DELETE FROM tenant WHERE id = $1`, row.tenantID},
	} {
		if stmt.arg == "" {
			continue
		}
		if _, err := pool.Exec(ctx, stmt.sql, stmt.arg); err != nil {
			t.Logf("credential cleanup (%s): %v", stmt.arg, err)
		}
	}
}

func TestStoreReadsACredentialWithItsRoleCapabilities(t *testing.T) {
	store := credentialStore(t)
	expires := time.Now().Add(time.Hour).UTC().Truncate(time.Microsecond)
	row := seedCredential(t, credentialSpec{
		role:           "producer",
		subjectPattern: ptr("checkout/*"),
		expiresAt:      &expires,
	})

	cred, err := store.CredentialByPrefix(context.Background(), row.prefix)
	require.NoError(t, err)

	assert.Equal(t, row.credentialID, cred.ID)
	assert.Equal(t, row.principalID, cred.PrincipalID)
	assert.Equal(t, "machine_key", cred.Type)
	assert.Equal(t, "producer", cred.Role)
	assert.Equal(t, []auth.Capability{auth.CapAppend}, cred.Capabilities,
		"capabilities come from the role table, not from the credential row")
	assert.Equal(t, row.tenantID, cred.Tenant.String())
	assert.False(t, cred.Tenant.IsZero())
	assert.Equal(t, "github", cred.Locus)
	assert.Equal(t, "checkout/*", cred.SubjectPattern)
	assert.Nil(t, cred.RevokedAt)
	require.NotNil(t, cred.ExpiresAt)
	assert.WithinDuration(t, expires, *cred.ExpiresAt, time.Second)
}

func TestStoreReadsEveryRoleInTheSeededVocabulary(t *testing.T) {
	store := credentialStore(t)
	for role, want := range map[string][]auth.Capability{
		"producer":     {auth.CapAppend},
		"reader":       {auth.CapRead},
		"tenant_admin": {auth.CapRead, auth.CapAdmin},
		"superuser":    {auth.CapAppend, auth.CapRead, auth.CapAdmin, auth.CapIssue},
	} {
		t.Run(role, func(t *testing.T) {
			row := seedCredential(t, credentialSpec{role: role})
			cred, err := store.CredentialByPrefix(context.Background(), row.prefix)
			require.NoError(t, err)
			assert.ElementsMatch(t, want, cred.Capabilities)
		})
	}
}

func TestStoreReadsANullTenantAsAGlobalPrincipal(t *testing.T) {
	store := credentialStore(t)
	row := seedCredential(t, credentialSpec{role: "superuser", global: true})

	cred, err := store.CredentialByPrefix(context.Background(), row.prefix)
	require.NoError(t, err)
	assert.True(t, cred.Tenant.IsZero(), "a NULL tenant_id is the credential that may select any")
	assert.Equal(t, "", cred.Tenant.String())
}

func TestStoreReadsANullSubjectPatternAsEmpty(t *testing.T) {
	store := credentialStore(t)
	row := seedCredential(t, credentialSpec{role: "reader"})

	cred, err := store.CredentialByPrefix(context.Background(), row.prefix)
	require.NoError(t, err)
	assert.Equal(t, "", cred.SubjectPattern)
	assert.Nil(t, cred.ExpiresAt, "a credential with no expiry never expires")
}

func TestStoreReportsErrNoCredentialForAnUnknownPrefix(t *testing.T) {
	store := credentialStore(t)
	_, err := store.CredentialByPrefix(context.Background(), "ffffffffffff")
	assert.ErrorIs(t, err, auth.ErrNoCredential,
		"the absence must be the one error the Authenticator caches")
}

func TestStoreReadsARevokedCredentialAsRevoked(t *testing.T) {
	store := credentialStore(t)
	row := seedCredential(t, credentialSpec{role: "reader"})
	execDB(t, `UPDATE credential SET revoked_at = now() WHERE id = $1`, row.credentialID)

	cred, err := store.CredentialByPrefix(context.Background(), row.prefix)
	require.NoError(t, err)
	require.NotNil(t, cred.RevokedAt, "revocation is a column the middleware has to see")
	assert.WithinDuration(t, time.Now(), *cred.RevokedAt, time.Minute)
}

// TestTheDatabaseAlsoRefusesASubjectPatternOnAnAdminGrant pins the backstop the
// middleware is the authority for: the CHECK in migration 0004 enumerates the roles it
// knows, and a role added later would slip past it into the middleware's own refusal.
// TestStoreRefusesARowItCannotReadAsAnUnknownCredential pins the fail-closed direction.
// An empty non-NULL tenant_id would read as the global credential — every tenant — so
// the row is refused; and it is refused as ErrNoCredential, because answering a store
// error here would tell a caller their prefix exists while an unknown one gets 401.
func TestStoreRefusesARowItCannotReadAsAnUnknownCredential(t *testing.T) {
	store := credentialStore(t)

	// tenant_id has a foreign key, so the empty tenant has to exist for a credential to
	// hold the value this guard is about. Seeded before the credential so that the
	// cleanups, which run last-registered first, drop the credential before the tenant.
	execDB(t, `INSERT INTO tenant (id, display_name) VALUES ('', 'the empty tenant')
		ON CONFLICT (id) DO NOTHING`)
	t.Cleanup(func() { execDB(t, `DELETE FROM tenant WHERE id = ''`) })

	row := seedCredential(t, credentialSpec{role: "reader"})
	execDB(t, `UPDATE credential SET tenant_id = '' WHERE id = $1`, row.credentialID)

	_, err := store.CredentialByPrefix(context.Background(), row.prefix)
	require.Error(t, err)
	assert.ErrorIs(t, err, auth.ErrNoCredential,
		"an unusable row is a credential failure, so it cannot answer 503 where an "+
			"unknown prefix answers 401")
}

func TestTheDatabaseAlsoRefusesASubjectPatternOnAnAdminGrant(t *testing.T) {
	credentialStore(t)
	row := seedCredential(t, credentialSpec{role: "reader"})
	pool, err := testdb.Pool()
	require.NoError(t, err)

	_, err = pool.Exec(context.Background(), `
		UPDATE credential SET role = 'tenant_admin', subject_pattern = 'checkout/*'
		WHERE id = $1`, row.credentialID)
	assert.Error(t, err, "credential_admin_has_no_subject_pattern must refuse it")
}
