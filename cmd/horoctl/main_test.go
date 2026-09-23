package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
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

// TestDelegatedPluginExitCodeIsPreserved pins exit-code fidelity through delegation: a
// caller scripting `horoctl weather; case $? in 3) ... esac` must see the plugin's own
// exit code, not a fixed 1, and must not see horoctl's own wrapper text on top of
// whatever the plugin already wrote to stderr.
func TestDelegatedPluginExitCodeIsPreserved(t *testing.T) {
	dir := t.TempDir()
	writeExecutable(t, filepath.Join(dir, "horoctl-flaky"),
		"#!/bin/sh\necho plugin reported this on stderr >&2\nexit 3\n")
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))

	_, stderr, err := execHoroctl(t, "flaky")
	require.Error(t, err)
	var exitErr *exec.ExitError
	require.ErrorAs(t, err, &exitErr)
	assert.Equal(t, 3, exitErr.ExitCode())
	assert.Contains(t, stderr, "plugin reported this on stderr",
		"the plugin's own stderr must still pass through")
	assert.NotContains(t, stderr, "exit status",
		"the plugin already reported; horoctl must not add its own wrapper line on top")
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

// devExplicitKeyPrefix is a valid-hex, 12-character prefix: parseExplicitKey validates an
// explicit key against the identical rule auth.SplitKey enforces on a presented
// credential, so a fixture prefix has to satisfy that rule to ever be usable, the same as
// one auth.NewKey would generate.
const devExplicitKeyPrefix = "deaddeaddead"

func TestIssueWithAnExplicitKeyWarns(t *testing.T) {
	// The key's prefix is fixed by the test, not generated, so a second run in the same
	// shared database must not collide with the first run's row.
	resetCredential(t, devExplicitKeyPrefix)
	_, stderr := runHoroctlSplit(t, "admin", "keys", "issue",
		"--tenant", tenantID, "--role", "producer", "--source", "dev",
		"--key", "horo_"+devExplicitKeyPrefix+"_"+strings.Repeat("a", 64))
	assert.Contains(t, stderr, "development only")
}

func TestIssueRejectsAShortExplicitKey(t *testing.T) {
	_, err := runHoroctlErr(t, "admin", "keys", "issue",
		"--tenant", tenantID, "--role", "producer", "--source", "dev", "--key", "short")
	require.Error(t, err)
}

// TestIssueRejectsANonHexExplicitKeyPrefix pins the fix for a credential that would
// otherwise insert cleanly and then never authenticate: auth.SplitKey requires a 12-
// character hex prefix, and a --key that does not match it must be refused at issue time
// with a message naming the requirement, not stored as a credential nothing can present.
func TestIssueRejectsANonHexExplicitKeyPrefix(t *testing.T) {
	out, err := runHoroctlErr(t, "admin", "keys", "issue",
		"--tenant", tenantID, "--role", "producer", "--source", "dev",
		"--key", "horo_devdevdevdev_"+strings.Repeat("a", 64))
	require.Error(t, err)
	assert.Contains(t, out, "auth.SplitKey",
		"the error must name the requirement an operator has to satisfy")
}

// TestIssueRejectsAMalformedExplicitKeyWithoutEchoingIt pins the fix for a malformed --key
// being embedded verbatim in the rejection error: that error reaches stderr unmodified from
// every caller (horoctl's own dispatch, bootstrap's ensureCredential run on every compose
// stack start, and keys issue's deriveKey), so a mistyped or truncated secret must never
// travel into a log stream through it.
func TestIssueRejectsAMalformedExplicitKeyWithoutEchoingIt(t *testing.T) {
	const secretMarker = "shouldneverbeloggedmarker"
	out, err := runHoroctlErr(t, "admin", "keys", "issue",
		"--tenant", tenantID, "--role", "producer", "--source", "dev",
		"--key", "horo_devdevdevdev_"+secretMarker+strings.Repeat("a", 64))
	require.Error(t, err)
	assert.NotContains(t, out, secretMarker,
		"the rejected key must not be echoed back into the error")
}

func TestTenantCreateMintsAnOpaqueID(t *testing.T) {
	stdout, _ := runHoroctlSplit(t, "admin", "tenant", "create", "--display-name", "acme")
	id := strings.TrimSpace(stdout)
	assert.True(t, strings.HasPrefix(id, "tnt_"))
	assert.NotContains(t, id, "acme", "the id is opaque; the display name is not the id")
}

// devProducerPrefix and devReaderPrefix are valid-hex, 12-character prefixes — the shape
// auth.SplitKey requires of any credential that is to authenticate a real request, dev
// convenience or not.
const (
	devProducerPrefix = "dead00000001"
	devProducerKey    = "horo_" + devProducerPrefix + "_" +
		"00000000000000000000000000000000000000000000000000000000000000"
	devReaderPrefix = "dead00000002"
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

// tenantCountByName counts tenant rows named displayName outright, unscoped by any
// credential. That is safe only for a name this test minted itself and never reuses —
// unlike tenantCount's "default", which tenantCount's own doc explains is shared across
// runs of this suite by design and therefore needs scoping through a credential.
func tenantCountByName(t *testing.T, displayName string) int {
	t.Helper()
	pool, err := testdb.Pool()
	require.NoError(t, err)
	var n int
	require.NoError(t, pool.QueryRow(context.Background(),
		`SELECT count(*) FROM tenant WHERE display_name = $1`, displayName).Scan(&n))
	return n
}

// newExplicitKey mints a fresh, valid horo_<prefix>_<secret> key via auth.NewKey, for a
// test that needs bootstrap's --producer-key/--reader-key shape without colliding with
// devProducerKey/devReaderKey or any other test's fixed prefix.
func newExplicitKey(t *testing.T) (key, prefix string) {
	t.Helper()
	key, prefix, _, err := auth.NewKey(rand.Reader)
	require.NoError(t, err)
	return key, prefix
}

// TestBootstrapUnderConcurrencySharesOneTenant runs two bootstrap invocations
// concurrently against the same, freshly minted display name and the same fixed
// producer/reader keys — the shape a retried CI job, an operator re-running while a
// prior run is in flight, or a Kubernetes Job with parallelism > 1 takes.
// TestBootstrapIsIdempotent above already covers two SEQUENTIAL runs, which is exactly
// why it never caught this: resolveTenant's check-then-act (SELECT, then INSERT if
// absent) races when both runs start with no tenant row yet. Without its advisory lock,
// both can see no match and both INSERT, leaving two tenants under one display name,
// and whichever run then binds its fixed key prefix to the "wrong" tenant_id collides
// with credential.key_prefix's UNIQUE constraint.
func TestBootstrapUnderConcurrencySharesOneTenant(t *testing.T) {
	if os.Getenv(testdb.DSNVar) == "" {
		t.Skip("set " + testdb.DSNVar + " to run horoctl tests (docker compose up -d db)")
	}
	name := "horoctl-concurrent-bootstrap-" + strconv.FormatInt(time.Now().UnixNano(), 36)
	producerKey, producerPrefix := newExplicitKey(t)
	readerKey, readerPrefix := newExplicitKey(t)
	t.Cleanup(func() {
		deleteCredential(producerPrefix)
		deleteCredential(readerPrefix)
	})
	args := []string{"admin", "bootstrap", "--display-name", name, "--source", "dev",
		"--locus", "dev", "--producer-key", producerKey, "--reader-key", readerKey}

	var wg sync.WaitGroup
	errs := make([]error, 2)
	stderrs := make([]string, 2)
	for i := range errs {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, stderrs[i], errs[i] = execHoroctl(t, args...)
		}(i)
	}
	wg.Wait()

	require.NoError(t, errs[0], "first concurrent bootstrap run: %s", stderrs[0])
	require.NoError(t, errs[1], "second concurrent bootstrap run: %s", stderrs[1])
	assert.Equal(t, 1, tenantCountByName(t, name))
	assert.Equal(t, 1, credentialCount(t, producerPrefix))
	assert.Equal(t, 1, credentialCount(t, readerPrefix))
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

// execDB runs one statement against the shared development database, for a test that has to
// put the database into a state horoctl itself will not create — residue an earlier run left.
func execDB(t *testing.T, sql string, args ...any) {
	t.Helper()
	pool, err := testdb.Pool()
	require.NoError(t, err)
	_, err = pool.Exec(context.Background(), sql, args...)
	require.NoError(t, err)
}

// credentialBinding reads back what a credential is bound to.
func credentialBinding(t *testing.T, prefix string) (tenantID, role string) {
	t.Helper()
	pool, err := testdb.Pool()
	require.NoError(t, err)
	require.NoError(t, pool.QueryRow(context.Background(),
		`SELECT COALESCE(tenant_id, ''), role FROM credential WHERE key_prefix = $1`,
		prefix).Scan(&tenantID, &role))
	return tenantID, role
}

// seedRole adds a role to the table and removes it afterwards. The name is unique per run
// because the shared development database is never truncated, and the capabilities are what
// the test is about — the name is not.
func seedRole(t *testing.T, capabilities []string) string {
	t.Helper()
	name := "auditor-" + strconv.FormatInt(time.Now().UnixNano(), 36)
	execDB(t, `INSERT INTO role (name, capabilities) VALUES ($1, $2)`, name, capabilities)
	t.Cleanup(func() { execDB(t, `DELETE FROM role WHERE name = $1`, name) })
	return name
}

// TestIssueRefusesASubjectPatternOnEveryAdminCapableRole is the composition the whole-branch
// review found: role is a table precisely so the vocabulary can grow, so an issue-time check
// that enumerates tenant_admin and superuser goes stale the moment a role is added. Such a
// credential inserted cleanly, printed a key, and then answered 401 on every request it ever
// made, because internal/auth refuses an admin-capable credential carrying a subject pattern.
// The check reads role.capabilities now, which is the same fact the middleware tests.
func TestIssueRefusesASubjectPatternOnEveryAdminCapableRole(t *testing.T) {
	role := seedRole(t, []string{"read", "admin"})
	out, err := runHoroctlErr(t, "admin", "keys", "issue", "--tenant", tenantID,
		"--role", role, "--source", "github", "--subject-pattern", "x/*")
	require.Error(t, err)
	assert.Contains(t, out, "grants admin")
	assert.NotContains(t, out, "horo_",
		"a credential that can never authenticate must not be minted, let alone printed")
}

// TestIssueAllowsASubjectPatternOnANewNonAdminRole is the other half: the check must follow
// the capabilities rather than refuse every unfamiliar name.
func TestIssueAllowsASubjectPatternOnANewNonAdminRole(t *testing.T) {
	role := seedRole(t, []string{"read"})
	stdout, _ := runHoroctlSplit(t, "admin", "keys", "issue", "--tenant", tenantID,
		"--role", role, "--source", "github", "--subject-pattern", "x/*")

	prefix, _, ok := auth.SplitKey(strings.TrimSpace(stdout))
	require.True(t, ok)
	cleanupCredential(t, prefix)
}

func TestIssueRejectsAnUnknownRole(t *testing.T) {
	out, err := runHoroctlErr(t, "admin", "keys", "issue", "--tenant", tenantID,
		"--role", "not-a-role", "--source", "github")
	require.Error(t, err)
	assert.Contains(t, out, "unknown role")
	assert.NotContains(t, out, "horo_", "an unknown role must fail before a key is minted")
}

// bootstrapFixture is one bootstrap invocation over prefixes and a tenant no other test
// shares, so a test may leave the credential in a state the dev stack's own keys must not be
// left in. Cleanup order is deliberate: the tenant row is registered for deletion first so it
// runs last, after the credentials whose foreign keys point at it.
type bootstrapFixture struct {
	args                         []string
	displayName                  string
	producerPrefix, readerPrefix string
}

func newBootstrapFixture(t *testing.T, producerPrefix, readerPrefix string) bootstrapFixture {
	t.Helper()
	name := "horoctl-bootstrap-" + strconv.FormatInt(time.Now().UnixNano(), 36)
	t.Cleanup(func() { execDB(t, `DELETE FROM tenant WHERE display_name = $1`, name) })
	resetCredential(t, producerPrefix)
	resetCredential(t, readerPrefix)

	key := func(prefix string) string { return "horo_" + prefix + "_" + strings.Repeat("a", 62) }
	return bootstrapFixture{
		args: []string{"admin", "bootstrap", "--display-name", name, "--source", "dev",
			"--locus", "dev", "--producer-key", key(producerPrefix),
			"--reader-key", key(readerPrefix)},
		displayName:    name,
		producerPrefix: producerPrefix,
		readerPrefix:   readerPrefix,
	}
}

// TestBootstrapReconcilesACredentialBoundElsewhere pins what "idempotent by design" has to
// mean. Treating "a credential with this prefix exists" as done left a key bound to whatever
// tenant and role an earlier run gave it: both daemons then authenticated against the stale
// row while bootstrap reported success, and because bootstrap runs on every container start
// nothing ever corrected it.
func TestBootstrapReconcilesACredentialBoundElsewhere(t *testing.T) {
	f := newBootstrapFixture(t, "beef00000001", "beef00000002")
	require.NoError(t, runHoroctlOK(t, f.args...))
	wantTenant, _ := credentialBinding(t, f.producerPrefix)

	execDB(t, `UPDATE credential SET tenant_id = $1, role = 'reader' WHERE key_prefix = $2`,
		tenantID, f.producerPrefix)

	_, stderr := runHoroctlSplit(t, f.args...)

	gotTenant, gotRole := credentialBinding(t, f.producerPrefix)
	assert.Equal(t, wantTenant, gotTenant, "a key left bound to another tenant must be rebound")
	assert.Equal(t, "producer", gotRole, "and to the role bootstrap issues it as")
	assert.Contains(t, stderr, "reconciled credential "+f.producerPrefix,
		"the correction must be visible, not silent")
	assert.Contains(t, stderr, "tenant ")
}

// TestBootstrapRefusesToUnRevokeACredential is the one mismatch bootstrap does not own.
// Clearing revoked_at on the next container start would undo an operator's deliberate act
// with nothing said, so it fails loudly and names the credential instead.
func TestBootstrapRefusesToUnRevokeACredential(t *testing.T) {
	f := newBootstrapFixture(t, "beef00000003", "beef00000004")
	require.NoError(t, runHoroctlOK(t, f.args...))
	execDB(t, `UPDATE credential SET revoked_at = now() WHERE key_prefix = $1`, f.producerPrefix)

	out, err := runHoroctlErr(t, f.args...)
	require.Error(t, err)
	assert.Contains(t, out, f.producerPrefix)
	assert.Contains(t, out, "revoked")
}
