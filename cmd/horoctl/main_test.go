package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/json"
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

// horoctlBin is the binary TestMain builds; every test runs it as a subprocess.
var horoctlBin string

// tenantID is a tenant created once for the whole suite.
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

// bootstrapSharedTenant takes no *testing.T: TestMain has none to hand it.
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

// execHoroctl runs the built binary, skipping when HOROPTER_TEST_DSN is unset.
func execHoroctl(t *testing.T, args ...string) (stdout, stderr string, err error) {
	t.Helper()
	dsn := os.Getenv(testdb.DSNVar)
	if dsn == "" {
		t.Skip("set " + testdb.DSNVar + " to run horoctl tests (docker compose up -d db)")
	}
	// horoctlBin is runTestMain's build; args are this file's own literals.
	//nolint:gosec // G204
	cmd := exec.Command(horoctlBin, args...)
	cmd.Env = append(os.Environ(), "DATABASE_URL="+dsn)
	var outBuf, errBuf bytes.Buffer
	cmd.Stdout, cmd.Stderr = &outBuf, &errBuf
	err = cmd.Run()
	return outBuf.String(), errBuf.String(), err
}

// runHoroctl runs horoctl, requires success and returns stdout.
func runHoroctl(t *testing.T, args ...string) string {
	t.Helper()
	stdout, _ := runHoroctlSplit(t, args...)
	return stdout
}

// runHoroctlSplit runs horoctl, requires success and returns stdout and stderr apart.
func runHoroctlSplit(t *testing.T, args ...string) (stdout, stderr string) {
	t.Helper()
	stdout, stderr, err := execHoroctl(t, args...)
	require.NoError(t, err, "horoctl %s failed; stderr: %s", strings.Join(args, " "), stderr)
	return stdout, stderr
}

// runHoroctlErr runs horoctl and returns its combined output and error.
func runHoroctlErr(t *testing.T, args ...string) (string, error) {
	t.Helper()
	stdout, stderr, err := execHoroctl(t, args...)
	return stdout + stderr, err
}

// runHoroctlOK runs horoctl and returns only the error.
func runHoroctlOK(t *testing.T, args ...string) error {
	t.Helper()
	_, _, err := execHoroctl(t, args...)
	return err
}

// writeExecutable stages a fake horoctl-<verb> plugin script at path.
func writeExecutable(t *testing.T, path, content string) {
	t.Helper()
	// A plugin stand-in must be executable; 0600 would defeat the fixture.
	//nolint:gosec // G306
	require.NoError(t, os.WriteFile(path, []byte(content), 0o755))
}

func TestSchemaTouchingVerbsAreNeverDelegated(t *testing.T) {
	dir := t.TempDir()
	// horoctl-migrate, not horopter-migrate: delegate() finds only that, else this passes vacuously.
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

// TestDelegatedPluginExitCodeIsPreserved: a plugin's exit code and stderr pass through unwrapped.
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

// TestIssueRejectsALegacySourceDefaultingToALegacyLocus: the locus defaults from --source.
func TestIssueRejectsALegacySourceDefaultingToALegacyLocus(t *testing.T) {
	out, err := runHoroctlErr(t, "admin", "keys", "issue",
		"--tenant", tenantID, "--role", "producer", "--source", "legacy")
	require.Error(t, err)
	assert.Contains(t, out, "legacy")
}

// devExplicitKeyPrefix is 12 hex characters, as auth.SplitKey requires of a usable prefix.
const devExplicitKeyPrefix = "deaddeaddead"

func TestIssueWithAnExplicitKeyWarns(t *testing.T) {
	// A fixed prefix: clear the row a previous run left in the shared database.
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

// TestIssueRejectsANonHexExplicitKeyPrefix: a key SplitKey refuses is refused at issue time.
func TestIssueRejectsANonHexExplicitKeyPrefix(t *testing.T) {
	out, err := runHoroctlErr(t, "admin", "keys", "issue",
		"--tenant", tenantID, "--role", "producer", "--source", "dev",
		"--key", "horo_devdevdevdev_"+strings.Repeat("a", 64))
	require.Error(t, err)
	assert.Contains(t, out, "auth.SplitKey",
		"the error must name the requirement an operator has to satisfy")
}

// TestIssueRejectsAMalformedExplicitKeyWithoutEchoingIt: the error must not leak the secret.
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

// devProducerPrefix and devReaderPrefix are 12 hex characters, as auth.SplitKey requires.
const (
	devProducerPrefix = "dead00000001"
	devProducerKey    = "horo_" + devProducerPrefix + "_" +
		"00000000000000000000000000000000000000000000000000000000000000"
	devReaderPrefix = "dead00000002"
	devReaderKey    = "horo_" + devReaderPrefix + "_" +
		"11111111111111111111111111111111111111111111111111111111111111"
)

// runBootstrap mirrors the compose stack: one producer, one reader, no superuser.
func runBootstrap(t *testing.T) {
	t.Helper()
	require.NoError(t, runHoroctlOK(t, "admin", "bootstrap", "--display-name", "default",
		"--source", "dev", "--locus", "dev",
		"--producer-key", devProducerKey, "--reader-key", devReaderKey))
}

// tenantCount scopes through devProducerPrefix's credential: display names are not unique.
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

// principalCount counts the principals issueCredential would derive from prefix.
func principalCount(t *testing.T, prefix string) int {
	t.Helper()
	pool, err := testdb.Pool()
	require.NoError(t, err)
	var n int
	require.NoError(t, pool.QueryRow(context.Background(),
		`SELECT count(*) FROM principal WHERE id = $1`, "prn_"+prefix).Scan(&n))
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

// tenantCountByName is unscoped, so it is safe only for a name the test minted.
func tenantCountByName(t *testing.T, displayName string) int {
	t.Helper()
	pool, err := testdb.Pool()
	require.NoError(t, err)
	var n int
	require.NoError(t, pool.QueryRow(context.Background(),
		`SELECT count(*) FROM tenant WHERE display_name = $1`, displayName).Scan(&n))
	return n
}

// newExplicitKey mints a valid key whose prefix no other test uses.
func newExplicitKey(t *testing.T) (key, prefix string) {
	t.Helper()
	key, prefix, _, err := auth.NewKey(rand.Reader)
	require.NoError(t, err)
	return key, prefix
}

// TestBootstrapUnderConcurrencySharesOneTenant races two bootstraps on one new display name.
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

// testCredential is one row allCredentials reads back.
type testCredential struct{ prefix, role string }

// allCredentials reads only bootstrap's two prefixes, not other suites' live rows.
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

// resetCredential deletes prefix's rows now and after the test: fixed prefixes recur.
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

// cleanupCredential deletes prefix's rows after the test only.
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

// TestAGlobalProducerCredentialIsIssuable: issue allows it; core.Event.Validate refuses it.
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

// execDB runs one statement to seed state horoctl itself will not create.
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

// seedRole adds a uniquely named role for the test and removes it afterwards.
func seedRole(t *testing.T, capabilities []string) string {
	t.Helper()
	name := "auditor-" + strconv.FormatInt(time.Now().UnixNano(), 36)
	execDB(t, `INSERT INTO role (name, capabilities) VALUES ($1, $2)`, name, capabilities)
	t.Cleanup(func() { execDB(t, `DELETE FROM role WHERE name = $1`, name) })
	return name
}

// TestIssueAllowsASubjectPatternOnProducer: append is what a pattern narrows.
func TestIssueAllowsASubjectPatternOnProducer(t *testing.T) {
	stdout, _ := runHoroctlSplit(t, "admin", "keys", "issue", "--tenant", tenantID,
		"--role", "producer", "--source", "github", "--subject-pattern", "team-a/*")

	prefix, _, ok := auth.SplitKey(strings.TrimSpace(stdout))
	require.True(t, ok)
	cleanupCredential(t, prefix)
}

// TestIssueRefusesASubjectPatternOnReader pins Finding 1: reads ignore subject patterns.
func TestIssueRefusesASubjectPatternOnReader(t *testing.T) {
	out, err := runHoroctlErr(t, "admin", "keys", "issue", "--tenant", tenantID,
		"--role", "reader", "--source", "github", "--subject-pattern", "team-a/*")
	require.Error(t, err)
	assert.Contains(t, out, "grants more than append", "the refusal must name why")
	assert.NotContains(t, out, "horo_",
		"a credential a pattern would silently do nothing for must not be minted")
}

// TestIssueRefusesASubjectPatternOnEveryAdminCapableRole: capabilities decide, not names.
func TestIssueRefusesASubjectPatternOnEveryAdminCapableRole(t *testing.T) {
	role := seedRole(t, []string{"read", "admin"})
	out, err := runHoroctlErr(t, "admin", "keys", "issue", "--tenant", tenantID,
		"--role", role, "--source", "github", "--subject-pattern", "x/*")
	require.Error(t, err)
	assert.Contains(t, out, "grants more than append")
	assert.NotContains(t, out, "horo_",
		"a credential that can never authenticate must not be minted, let alone printed")
}

// TestIssueAllowsASubjectPatternOnANewAppendOnlyRole: an unnamed append-only role qualifies.
func TestIssueAllowsASubjectPatternOnANewAppendOnlyRole(t *testing.T) {
	role := seedRole(t, []string{"append"})
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

// TestIssueWithANonexistentTenantLeavesNoOrphanedPrincipal pins Finding 3.
func TestIssueWithANonexistentTenantLeavesNoOrphanedPrincipal(t *testing.T) {
	key, prefix := newExplicitKey(t)
	cleanupCredential(t, prefix)

	out, err := runHoroctlErr(t, "admin", "keys", "issue",
		"--tenant", "tnt_does_not_exist", "--role", "producer", "--source", "test", "--key", key)
	require.Error(t, err)
	assert.Contains(t, out, "tnt_does_not_exist")
	assert.Contains(t, out, "does not exist")
	assert.Equal(t, 0, principalCount(t, prefix),
		"a failed issue must not leave an orphaned principal")
	assert.Equal(t, 0, credentialCount(t, prefix))
}

// bootstrapFixture bootstraps over prefixes and a tenant no other test shares.
type bootstrapFixture struct {
	args                         []string
	displayName                  string
	producerPrefix, readerPrefix string
}

func newBootstrapFixture(t *testing.T, producerPrefix, readerPrefix string) bootstrapFixture {
	t.Helper()
	name := "horoctl-bootstrap-" + strconv.FormatInt(time.Now().UnixNano(), 36)
	// Registered first so it runs last, after the credentials that reference the tenant.
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

// TestBootstrapReconcilesACredentialBoundElsewhere: a key left on another tenant is rebound.
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

// TestBootstrapRefusesToUnRevokeACredential: revocation is the operator's, not bootstrap's.
func TestBootstrapRefusesToUnRevokeACredential(t *testing.T) {
	f := newBootstrapFixture(t, "beef00000003", "beef00000004")
	require.NoError(t, runHoroctlOK(t, f.args...))
	execDB(t, `UPDATE credential SET revoked_at = now() WHERE key_prefix = $1`, f.producerPrefix)

	out, err := runHoroctlErr(t, f.args...)
	require.Error(t, err)
	assert.Contains(t, out, f.producerPrefix)
	assert.Contains(t, out, "revoked")
}

// TestResolveLocusDefaultsToSourceOnlyWhenLocusIsEmpty pins what swapped arguments broke.
func TestResolveLocusDefaultsToSourceOnlyWhenLocusIsEmpty(t *testing.T) {
	assert.Equal(t, "github", resolveLocus(issueFlags{source: "github"}),
		"an absent locus defaults to the source")
	assert.Equal(t, "prod", resolveLocus(issueFlags{source: "github", locus: "prod"}),
		"an explicit locus wins over the source")
	assert.Equal(t, "prod", resolveLocus(issueFlags{locus: "prod"}),
		"an explicit locus does not need a source; swapping the two would return the "+
			"source here instead")
	assert.Empty(t, resolveLocus(issueFlags{}),
		"neither given resolves to empty, which ValidateLocus then refuses")
}

func TestVersionAndBuildInfoAnswerBeforeAnyVerb(t *testing.T) {
	t.Setenv("PATH", t.TempDir())
	assert.True(t, strings.HasPrefix(runHoroctl(t, "--version"), "horoctl "),
		"compiled in, never looked up as a horoctl---version plugin")
	assert.Contains(t, runHoroctl(t, "--build-info", "--json"), `"name":"horoctl"`)
}

// TestAPluginIsToldTheVersionHoroctlReports: --build-info's version, without the v.
func TestAPluginIsToldTheVersionHoroctlReports(t *testing.T) {
	dir := t.TempDir()
	writeExecutable(t, filepath.Join(dir, "horoctl-whichversion"),
		"#!/bin/sh\necho \"$HOROCTL_VERSION\"\n")
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))

	var info struct{ Version string }
	require.NoError(t, json.Unmarshal([]byte(runHoroctl(t, "--build-info", "--json")), &info))
	require.NotEmpty(t, info.Version)
	assert.Equal(t, info.Version, strings.TrimSpace(runHoroctl(t, "whichversion")))
}
