package postgres_test

import (
	"os"
	"testing"

	"horopter.dev/horopter/internal/testdb"
)

// dsn returns the test database DSN, skipping loudly when it is absent.
func dsn(t *testing.T) string {
	t.Helper()
	v := os.Getenv(testdb.DSNVar)
	if v == "" {
		t.Skip("set " + testdb.DSNVar + " to run Postgres tests (docker compose up -d db)")
	}
	return v
}

// siblingTenant derives a second tenant from a test's own, for the tests that need two,
// and registers its cleanup alongside. Building the name by hand leaves the sibling
// behind: DropTenantAfter matches the exact tenant deliberately, because a prefix match
// would have to escape the LIKE metacharacters that Go subtest names legitimately contain.
func siblingTenant(t *testing.T, base, suffix string) string {
	t.Helper()
	tn := base + "-" + suffix
	testdb.DropTenantAfter(t, tn)
	return tn
}
