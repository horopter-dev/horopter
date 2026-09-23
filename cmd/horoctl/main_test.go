package main

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"horopter.dev/horopter/internal/auth"
	"horopter.dev/horopter/internal/testdb"
)

// horoctlBin is the path TestMain builds the real horoctl binary to. The dispatch tests
// need a genuine subprocess — PATH resolution and delegation happen in exec.LookPath and
// os/exec, neither of which a same-process call to main() could exercise — and every other
// test runs the built binary too, so the whole suite exercises exactly what an operator
// would run.
var horoctlBin string

// tenantID is a tenant created once for the whole suite, shared by every test that needs
// a real opaque id to pass as --tenant. keys issue's --tenant is opaque by design, so
// nothing here parses or depends on its shape beyond that.
var tenantID string

func TestMain(m *testing.M) {
	os.Exit(runTestMain(m))
}

func runTestMain(m *testing.M) int {
	tmp, err := os.MkdirTemp("", "horoctl-bin-*")
	if err != nil {
		fmt.Fprintln(os.Stderr, "horoctl test setup:", err)
		return 1
	}
	defer func() { _ = os.RemoveAll(tmp) }()

	horoctlBin = filepath.Join(tmp, "horoctl")
	// The command name is the fixed literal "go", not caller input.
	//nolint:gosec // G204
	build := exec.Command("go", "build", "-o", horoctlBin, ".")
	build.Stderr = os.Stderr
	if err := build.Run(); err != nil {
		fmt.Fprintln(os.Stderr, "build horoctl:", err)
		return 1
	}

	if dsn := os.Getenv(testdb.DSNVar); dsn != "" {
		id, err := bootstrapSharedTenant(dsn)
		if err != nil {
			fmt.Fprintln(os.Stderr, "create shared test tenant:", err)
			return 1
		}
		tenantID = id
	}

	return m.Run()
}

// bootstrapSharedTenant runs the freshly built binary directly, without a *testing.T:
// TestMain has none to hand a helper.
func bootstrapSharedTenant(dsn string) (string, error) {
	name := "horoctl-cli-test-" + strconv.FormatInt(time.Now().UnixNano(), 36)
	// horoctlBin is the path runTestMain just built, not caller input.
	//nolint:gosec // G204
	cmd := exec.Command(horoctlBin, "admin", "tenant", "create", "--display-name", name)
	cmd.Env = append(os.Environ(), "DATABASE_URL="+dsn)
	var out, errOut bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errOut
	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("%w: %s", err, errOut.String())
	}
	return strings.TrimSpace(out.String()), nil
}

// execHoroctl runs the built binary with args, returning its stdout and stderr
// separately. Every admin and migrate invocation needs a database, so this is the one
// place that skips when HOROPTER_TEST_DSN is unset — matching internal/testdb's own
// per-test skip, and covering the dispatch tests too, which need no database themselves
// but share this helper for consistency.
func execHoroctl(t *testing.T, args ...string) (stdout, stderr string, err error) {
	t.Helper()
	dsn := os.Getenv(testdb.DSNVar)
	if dsn == "" {
		t.Skip("set " + testdb.DSNVar + " to run horoctl tests (docker compose up -d db)")
	}
	// horoctlBin is the path runTestMain built; args come from this test file's own
	// literals, never from outside input.
	//nolint:gosec // G204
	cmd := exec.Command(horoctlBin, args...)
	cmd.Env = append(os.Environ(), "DATABASE_URL="+dsn)
	var outBuf, errBuf bytes.Buffer
	cmd.Stdout, cmd.Stderr = &outBuf, &errBuf
	err = cmd.Run()
	return outBuf.String(), errBuf.String(), err
}

// runHoroctl runs horoctl and requires success, returning stdout alone — for assertions
// that only care what a passing run printed there.
func runHoroctl(t *testing.T, args ...string) string {
	t.Helper()
	stdout, _ := runHoroctlSplit(t, args...)
	return stdout
}

// runHoroctlSplit runs horoctl, requires success, and returns stdout and stderr
// separately — for assertions that care which stream carried what, such as the key
// appearing on stdout exactly once with everything else on stderr.
func runHoroctlSplit(t *testing.T, args ...string) (stdout, stderr string) {
	t.Helper()
	stdout, stderr, err := execHoroctl(t, args...)
	require.NoError(t, err, "horoctl %s failed; stderr: %s", strings.Join(args, " "), stderr)
	return stdout, stderr
}

// runHoroctlErr runs horoctl without requiring anything, returning combined output
// alongside the error — for assertions that expect the run to fail.
func runHoroctlErr(t *testing.T, args ...string) (string, error) {
	t.Helper()
	stdout, stderr, err := execHoroctl(t, args...)
	return stdout + stderr, err
}

// runHoroctlOK runs horoctl and returns only the error, for callers checking success or
// failure without needing the output — TestBootstrapIsIdempotent runs bootstrap twice and
// only cares that neither call fails.
func runHoroctlOK(t *testing.T, args ...string) error {
	t.Helper()
	_, _, err := execHoroctl(t, args...)
	return err
}

// writeExecutable creates a shell script at path and marks it executable, for staging a
// fake horoctl-<verb> plugin on a test's PATH.
func writeExecutable(t *testing.T, path, content string) {
	t.Helper()
	// The script must be executable to stand in for a horoctl-<verb> plugin on PATH;
	// 0600 would defeat the point of the fixture.
	//nolint:gosec // G306
	require.NoError(t, os.WriteFile(path, []byte(content), 0o755))
}

func TestSchemaTouchingVerbsAreNeverDelegated(t *testing.T) {
	dir := t.TempDir()
	writeExecutable(t, filepath.Join(dir, "horoctl-migrate"), "#!/bin/sh\necho hijacked\n")
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))

	out := runHoroctl(t, "migrate", "--help")
	assert.NotContains(t, out, "hijacked",
		"migrate is compiled in; a PATH binary must never be able to answer for it")
}

func TestAnUnknownVerbIsDelegatedToPATH(t *testing.T) {
	dir := t.TempDir()
	writeExecutable(t, filepath.Join(dir, "horoctl-weather"), "#!/bin/sh\necho sunny\n")
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))

	assert.Contains(t, runHoroctl(t, "weather"), "sunny")
}

func TestAnUnknownVerbWithNoPluginFails(t *testing.T) {
	t.Setenv("PATH", t.TempDir())
	out, err := runHoroctlErr(t, "nosuchverb")
	require.Error(t, err)
	assert.Contains(t, out, "unknown command \"nosuchverb\"")
}

func TestIssueRejectsALocusContainingASlash(t *testing.T) {
	out, err := runHoroctlErr(t, "admin", "keys", "issue",
		"--tenant", tenantID, "--role", "producer", "--source", "github",
		"--locus", "github/actions")
	require.Error(t, err)
	assert.Contains(t, out, "unsplittable")
}

func TestIssueWithAnExplicitKeyWarns(t *testing.T) {
	// The key's prefix ("devdevdevdev") is fixed by the test, not generated, so a second
	// run in the same shared database must not collide with the first run's row.
	resetCredential(t, "devdevdevdev")
	_, stderr := runHoroctlSplit(t, "admin", "keys", "issue",
		"--tenant", tenantID, "--role", "producer", "--source", "dev",
		"--key", "horo_devdevdevdev_"+strings.Repeat("a", 64))
	assert.Contains(t, stderr, "development only")
}

func TestIssueRejectsAShortExplicitKey(t *testing.T) {
	_, err := runHoroctlErr(t, "admin", "keys", "issue",
		"--tenant", tenantID, "--role", "producer", "--source", "dev", "--key", "short")
	require.Error(t, err)
}

func TestTenantCreateMintsAnOpaqueID(t *testing.T) {
	stdout, _ := runHoroctlSplit(t, "admin", "tenant", "create", "--display-name", "acme")
	id := strings.TrimSpace(stdout)
	assert.True(t, strings.HasPrefix(id, "tnt_"))
	assert.NotContains(t, id, "acme", "the id is opaque; the display name is not the id")
}

const (
	devProducerPrefix = "dev-producer-key"
	devProducerKey    = "horo_" + devProducerPrefix + "_" +
		"00000000000000000000000000000000000000000000000000000000000000"
	devReaderPrefix = "dev-reader-key"
	devReaderKey    = "horo_" + devReaderPrefix + "_" +
		"11111111111111111111111111111111111111111111111111111111111111"
)

// runBootstrap runs `admin bootstrap` with fixed dev credentials, matching the compose
// stack's own two-key convention: one producer, one reader, neither superuser.
func runBootstrap(t *testing.T) {
	t.Helper()
	require.NoError(t, runHoroctlOK(t, "admin", "bootstrap", "--display-name", "default",
		"--source", "dev", "--locus", "dev",
		"--producer-key", devProducerKey, "--reader-key", devReaderKey))
}

// tenantCount counts tenants named displayName that are also the tenant this suite's own
// bootstrap producer credential resolved to. A plain `WHERE display_name = $1` would be
// wrong in this codebase on principle, not only under pollution: display names are "not
// unique by design" (decisions/2026-09-22-tenant-identity-and-where-credentials-attach.md),
// so nothing stops another tenant, from another run or another suite, sharing the name
// "default" in the same never-truncated development database. Scoping through
// devProducerPrefix's credential — whose key_prefix is UNIQUE — names the one tenant this
// test's own bootstrap call actually resolved, however many other rows share its name.
func tenantCount(t *testing.T, displayName string) int {
	t.Helper()
	pool, err := testdb.Pool()
	require.NoError(t, err)
	var n int
	require.NoError(t, pool.QueryRow(context.Background(), `
		SELECT count(*) FROM tenant
		WHERE display_name = $1
		  AND id = (SELECT tenant_id FROM credential WHERE key_prefix = $2)`,
		displayName, devProducerPrefix).Scan(&n))
	return n
}

func credentialCount(t *testing.T, prefix string) int {
	t.Helper()
	pool, err := testdb.Pool()
	require.NoError(t, err)
	var n int
	require.NoError(t, pool.QueryRow(context.Background(),
		`SELECT count(*) FROM credential WHERE key_prefix = $1`, prefix).Scan(&n))
	return n
}

func TestBootstrapIsIdempotent(t *testing.T) {
	args := []string{"admin", "bootstrap", "--display-name", "default", "--source", "dev",
		"--locus", "dev", "--producer-key", devProducerKey, "--reader-key", devReaderKey}
	require.NoError(t, runHoroctlOK(t, args...))
	require.NoError(t, runHoroctlOK(t, args...), "task up twice must not fail")
	assert.Equal(t, 1, tenantCount(t, "default"))
	assert.Equal(t, 1, credentialCount(t, devProducerPrefix))
	assert.Equal(t, 1, credentialCount(t, devReaderPrefix))
}

// testCredential is one row allCredentials reads back, carrying just enough to look up
// its role's capabilities.
type testCredential struct{ prefix, role string }

// allCredentials reads back exactly the two rows bootstrap issues — devProducerPrefix and
// devReaderPrefix — rather than the whole credential table, so a credential some other
// package's test suite happens to have live in the shared database at the same moment
// cannot make this test's outcome depend on scheduling.
func allCredentials(t *testing.T) []testCredential {
	t.Helper()
	pool, err := testdb.Pool()
	require.NoError(t, err)
	rows, err := pool.Query(context.Background(),
		`SELECT key_prefix, role FROM credential WHERE key_prefix IN ($1, $2)`,
		devProducerPrefix, devReaderPrefix)
	require.NoError(t, err)
	defer rows.Close()

	var out []testCredential
	for rows.Next() {
		var c testCredential
		require.NoError(t, rows.Scan(&c.prefix, &c.role))
		out = append(out, c)
	}
	require.NoError(t, rows.Err())
	return out
}

func rolesOf(t *testing.T, creds []testCredential) []string {
	t.Helper()
	seen := map[string]bool{}
	var roles []string
	for _, c := range creds {
		if !seen[c.role] {
			seen[c.role] = true
			roles = append(roles, c.role)
		}
	}
	return roles
}

func capabilitiesOf(t *testing.T, role string) []string {
	t.Helper()
	pool, err := testdb.Pool()
	require.NoError(t, err)
	var caps []string
	require.NoError(t, pool.QueryRow(context.Background(),
		`SELECT capabilities FROM role WHERE name = $1`, role).Scan(&caps))
	return caps
}

func TestBootstrapIssuesNoCredentialThatCanIssue(t *testing.T) {
	runBootstrap(t)
	for _, role := range rolesOf(t, allCredentials(t)) {
		assert.NotContains(t, capabilitiesOf(t, role), "issue",
			"decision 10: a long-lived machine key must never carry management capability")
	}
}

func TestIssuePrintsTheKeyExactlyOnce(t *testing.T) {
	stdout, _ := runHoroctlSplit(t, "admin", "keys", "issue",
		"--tenant", tenantID, "--role", "producer", "--source", "github", "--locus", "github")
	assert.Equal(t, 1, strings.Count(stdout, "horo_"),
		"the secret is unrecoverable after this; printing it twice invites copying the wrong one")

	prefix, _, ok := auth.SplitKey(strings.TrimSpace(stdout))
	require.True(t, ok)
	cleanupCredential(t, prefix)
}

// resetCredential deletes any credential and principal rows for prefix immediately, and
// registers the same delete to run again after the test. The immediate delete matters for
// a fixed, non-random prefix (TestIssueWithAnExplicitKeyWarns's "devdevdevdev"): without
// it, a second run of this suite against the same shared, never-truncated database would
// collide with the first run's row rather than only with rows created within one test.
func resetCredential(t *testing.T, prefix string) {
	t.Helper()
	deleteCredential(prefix)
	t.Cleanup(func() { deleteCredential(prefix) })
}

func deleteCredential(prefix string) {
	pool, err := testdb.Pool()
	if err != nil {
		return
	}
	ctx := context.Background()
	_, _ = pool.Exec(ctx, `DELETE FROM credential WHERE key_prefix = $1`, prefix)
	_, _ = pool.Exec(ctx, `DELETE FROM principal WHERE id = $1`, "prn_"+prefix)
}

// cleanupCredential registers prefix's credential and principal rows for deletion after
// the test, without deleting anything now — for a credential the test just issued under a
// generated (never-colliding) prefix and still needs to exist for the rest of the test
// body, unlike resetCredential's fixed-prefix case above.
func cleanupCredential(t *testing.T, prefix string) {
	t.Helper()
	t.Cleanup(func() { deleteCredential(prefix) })
}

func TestKeysListShowsAnIssuedCredential(t *testing.T) {
	stdout, _ := runHoroctlSplit(t, "admin", "keys", "issue",
		"--tenant", tenantID, "--role", "reader", "--source", "test", "--locus", "list-test")
	prefix, _, ok := auth.SplitKey(strings.TrimSpace(stdout))
	require.True(t, ok)
	cleanupCredential(t, prefix)

	listOut := runHoroctl(t, "admin", "keys", "list", "--tenant", tenantID)
	assert.Contains(t, listOut, prefix)
	assert.Contains(t, listOut, "role=reader")
}

func TestKeysRevokeMarksTheCredentialRevoked(t *testing.T) {
	stdout, _ := runHoroctlSplit(t, "admin", "keys", "issue",
		"--tenant", tenantID, "--role", "reader", "--source", "test", "--locus", "revoke-test")
	prefix, _, ok := auth.SplitKey(strings.TrimSpace(stdout))
	require.True(t, ok)
	cleanupCredential(t, prefix)

	runHoroctlSplit(t, "admin", "keys", "revoke", "--prefix", prefix)

	pool, err := testdb.Pool()
	require.NoError(t, err)
	var revokedAt *time.Time
	require.NoError(t, pool.QueryRow(context.Background(),
		`SELECT revoked_at FROM credential WHERE key_prefix = $1`, prefix).Scan(&revokedAt))
	require.NotNil(t, revokedAt)
}

func TestKeysRevokeFailsForAnUnknownPrefix(t *testing.T) {
	_, err := runHoroctlErr(t, "admin", "keys", "revoke", "--prefix", "no-such-prefix")
	require.Error(t, err)
}

func TestTenantListShowsACreatedTenant(t *testing.T) {
	name := "horoctl-list-test-" + strconv.FormatInt(time.Now().UnixNano(), 36)
	runHoroctlSplit(t, "admin", "tenant", "create", "--display-name", name)

	out := runHoroctl(t, "admin", "tenant", "list")
	assert.Contains(t, out, name)
}

// TestAGlobalProducerCredentialIsIssuable re-confirms, now that keys issue can actually
// mint one, an answer earlier tasks only inferred: nothing at issue time rejects a
// credential that is both global (no tenant) and producer (append). core.Event.Validate's
// tenant check is where such a credential is refused, on the first append it attempts —
// see internal/core/event.go and this task's report for the traced behaviour.
func TestAGlobalProducerCredentialIsIssuable(t *testing.T) {
	stdout, _ := runHoroctlSplit(t, "admin", "keys", "issue",
		"--role", "producer", "--source", "test", "--locus", "global-test")
	key := strings.TrimSpace(stdout)
	prefix, _, ok := auth.SplitKey(key)
	require.True(t, ok)
	cleanupCredential(t, prefix)

	pool, err := testdb.Pool()
	require.NoError(t, err)
	var storedTenant *string
	require.NoError(t, pool.QueryRow(context.Background(),
		`SELECT tenant_id FROM credential WHERE key_prefix = $1`, prefix).Scan(&storedTenant))
	assert.Nil(t, storedTenant, "a global credential's tenant_id must be NULL")
}
