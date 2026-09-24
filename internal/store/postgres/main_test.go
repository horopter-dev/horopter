package postgres_test

import (
	"os"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"horopter.dev/horopter/internal/core"
	"horopter.dev/horopter/internal/store/postgres"
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
// creates its row, and registers its cleanup alongside. Building the name by hand leaves
// the sibling behind: DropTenantAfter matches the exact tenant deliberately, because a
// prefix match would have to escape the LIKE metacharacters that Go subtest names
// legitimately contain.
func siblingTenant(t *testing.T, base, suffix string) string {
	t.Helper()
	return testdb.NewTenant(t, base+"-"+suffix)
}

// changeQuery builds a tenant-scoped ChangeQuery. NewChangeQuery refuses the zero TenantID,
// which no caller here passes, so the error is asserted once here rather than at every site.
func changeQuery(
	t *testing.T, tn string, from, to time.Time, limit int, attrs map[string]string,
) postgres.ChangeQuery {
	t.Helper()
	q, err := postgres.NewChangeQuery(core.TenantIDFromRow(tn), from, to, limit, attrs)
	require.NoError(t, err)
	return q
}

// eventQuery builds a tenant-scoped EventQuery, as changeQuery does for changes.
func eventQuery(t *testing.T, tn string, limit int) postgres.EventQuery {
	t.Helper()
	q, err := postgres.NewEventQuery(core.TenantIDFromRow(tn), limit)
	require.NoError(t, err)
	return q
}

// refQuery is eventQuery scoped to one projection ref.
func refQuery(t *testing.T, tn, ref string) postgres.EventQuery {
	t.Helper()
	q := eventQuery(t, tn, 10)
	q.Ref = ref
	return q
}

// cutEventQuery is eventQuery with a row-text budget applied.
func cutEventQuery(t *testing.T, tn string, limit int, budget int64) postgres.EventQuery {
	t.Helper()
	q := eventQuery(t, tn, limit)
	q.MaxRowTextBytes = budget
	return q
}
