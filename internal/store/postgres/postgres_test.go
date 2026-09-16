package postgres_test

import (
	"context"
	"fmt"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/yo61/horopter/internal/store/postgres"
	"go.opentelemetry.io/otel/attribute"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
)

func TestOpenAppliesMigrations(t *testing.T) {
	ctx := context.Background()
	s, err := postgres.Open(ctx, dsn(t))
	require.NoError(t, err)
	t.Cleanup(s.Close)

	for _, table := range []string{"event", "change"} {
		var exists bool
		err := s.Pool().QueryRow(ctx,
			`SELECT EXISTS (SELECT 1 FROM information_schema.tables
			 WHERE table_schema = 'public' AND table_name = $1)`, table).Scan(&exists)
		require.NoError(t, err)
		require.True(t, exists, "table %q must exist after migration", table)
	}
}

// isolatedSchemaDSN creates a uniquely named, empty schema on the target database and
// returns a DSN whose search_path points at it, then drops the schema in t.Cleanup.
//
// TestOpenAppliesMigrations runs first (Go runs tests in declaration order) and leaves
// the public schema fully migrated, so racing Open calls against public would make every
// CREATE ... IF NOT EXISTS a no-op existence check with nothing left to collide over —
// the advisory lock could be deleted and the race would still pass. Racing against a
// schema of our own that has never held these tables is what makes the concurrent
// CREATE TABLE / CREATE INDEX statements a genuine catalog race.
func isolatedSchemaDSN(t *testing.T, base string) string {
	t.Helper()
	ctx := context.Background()

	admin, err := pgxpool.New(ctx, base)
	require.NoError(t, err)
	t.Cleanup(admin.Close)

	schema := fmt.Sprintf("migrace_%d", time.Now().UnixNano())
	ident := pgx.Identifier{schema}.Sanitize()
	_, err = admin.Exec(ctx, "CREATE SCHEMA "+ident)
	require.NoError(t, err)
	t.Cleanup(func() {
		_, _ = admin.Exec(context.Background(), "DROP SCHEMA "+ident+" CASCADE")
	})

	u, err := url.Parse(base)
	require.NoError(t, err)
	q := u.Query()
	q.Set("search_path", schema)
	u.RawQuery = q.Encode()
	return u.String()
}

func TestMigrationsAreIdempotentAndConcurrencySafe(t *testing.T) {
	ctx := context.Background()
	// dsn(t) must run on the test's own goroutine: t.Skip calls runtime.Goexit,
	// which only ends the calling goroutine rather than the test, so calling it
	// from inside the workers below would hang the <-errs loop forever instead
	// of skipping when HOROPTER_TEST_DSN is unset.
	base := dsn(t)
	d := isolatedSchemaDSN(t, base)

	// Ten simultaneous starts against a schema that exists but has never been
	// migrated, as N pods rolling out together against a freshly provisioned
	// database would produce. Without the advisory lock these race the same DDL.
	errs := make(chan error, 10)
	for i := 0; i < 10; i++ {
		go func() {
			s, err := postgres.Open(ctx, d)
			if err != nil {
				errs <- err
				return
			}
			s.Close()
			errs <- nil
		}()
	}
	for i := 0; i < 10; i++ {
		require.NoError(t, <-errs, "concurrent migration must not fail")
	}
}

// TestMigrationLedgerAppliesEachFileOnce is the guard that lets a non-idempotent
// migration exist at all. Without the applied_migration ledger every file re-executes on
// every run, which is harmless only while every migration is written with IF NOT EXISTS;
// the first backfill UPDATE or schema transformation would error on the second run or,
// worse, silently re-apply — data corruption in a system of record.
//
// The second Open SUCCEEDING is itself the proof, not a formality: 0002 adds a CHECK
// constraint by name, which Postgres refuses to add twice. The subtest below runs that
// statement again by hand to show the failure the ledger is preventing, so this test
// cannot pass vacuously if the migrations ever all become idempotent again.
func TestMigrationLedgerAppliesEachFileOnce(t *testing.T) {
	ctx := context.Background()
	d := isolatedSchemaDSN(t, dsn(t))

	first, err := postgres.Open(ctx, d)
	require.NoError(t, err)
	t.Cleanup(first.Close)

	applied := func() map[string]time.Time {
		rows, err := first.Pool().Query(ctx,
			`SELECT filename, applied_at FROM applied_migration ORDER BY filename`)
		require.NoError(t, err)
		defer rows.Close()
		out := map[string]time.Time{}
		for rows.Next() {
			var (
				name string
				at   time.Time
			)
			require.NoError(t, rows.Scan(&name, &at))
			out[name] = at
		}
		require.NoError(t, rows.Err())
		return out
	}

	afterFirst := applied()
	require.NotEmpty(t, afterFirst, "the ledger must record every migration it applied")
	require.Contains(t, afterFirst, "0001_init.sql")
	require.Contains(t, afterFirst, "0002_constrain_status_and_generate_end_effective.sql")

	second, err := postgres.Open(ctx, d)
	require.NoError(t, err,
		"a second migration run must skip every recorded file; 0002 cannot be applied twice")
	t.Cleanup(second.Close)

	assert.Equal(t, afterFirst, applied(),
		"a recorded migration must not re-execute, and its ledger row must not be rewritten")

	t.Run("0002 is genuinely not idempotent", func(t *testing.T) {
		_, err := second.Pool().Exec(ctx, `
			ALTER TABLE change ADD CONSTRAINT change_status_vocabulary
			    CHECK (status IN ('occurred', 'in_progress', 'completed', 'failed',
			                      'rolled_back', 'cancelled'))`)
		require.Error(t, err,
			"if this statement were idempotent the ledger test above would prove nothing")
	})
}

// TestSchemaVersionIsDerivedFromTheHighestMigration pins the version to the migrations
// actually present. It used to be a literal INSERT inside 0001_init.sql, so adding a
// migration without editing that literal left the database claiming a version its schema
// no longer matched and nothing noticed.
func TestSchemaVersionIsDerivedFromTheHighestMigration(t *testing.T) {
	ctx := context.Background()
	s, err := postgres.Open(ctx, isolatedSchemaDSN(t, dsn(t)))
	require.NoError(t, err)
	t.Cleanup(s.Close)

	var highest string
	require.NoError(t, s.Pool().QueryRow(ctx,
		`SELECT max(filename) FROM applied_migration`).Scan(&highest))

	v, err := s.SchemaVersion(ctx)
	require.NoError(t, err)
	assert.Equal(t, postgres.ExpectedSchemaVersion, v,
		"ExpectedSchemaVersion must be raised in the same change that adds a migration")
	assert.Equal(t, fmt.Sprintf("%04d", v), highest[:4],
		"the recorded version must be the numeric prefix of the highest applied migration")
}

// poolAttributeSets returns the attribute set of every data point on every pgxpool.*
// metric. otelpgx records pool statistics as int64 observable counters, up-down counters and
// a gauge, which arrive as Sum[int64] and Gauge[int64].
func poolAttributeSets(t *testing.T, reader *sdkmetric.ManualReader) []attribute.Set {
	t.Helper()
	var rm metricdata.ResourceMetrics
	require.NoError(t, reader.Collect(context.Background(), &rm))
	var sets []attribute.Set
	for _, scope := range rm.ScopeMetrics {
		for _, m := range scope.Metrics {
			if !strings.HasPrefix(m.Name, "pgxpool.") {
				continue
			}
			switch data := m.Data.(type) {
			case metricdata.Sum[int64]:
				sets = append(sets, pointAttributes(data.DataPoints)...)
			case metricdata.Gauge[int64]:
				sets = append(sets, pointAttributes(data.DataPoints)...)
			}
		}
	}
	return sets
}

func pointAttributes(points []metricdata.DataPoint[int64]) []attribute.Set {
	sets := make([]attribute.Set, 0, len(points))
	for _, point := range points {
		sets = append(sets, point.Attributes)
	}
	return sets
}

// TestStoreRecordsPoolStatisticsUnderAFixedPoolName pins the override on otelpgx's default
// pool name, host:port/database. Left alone, /metrics would publish the database's address
// and name to whoever can reach it — the detail /readyz was changed to stop returning.
func TestStoreRecordsPoolStatisticsUnderAFixedPoolName(t *testing.T) {
	reader := recordMetrics(t)
	newStore(t)

	cfg, err := pgxpool.ParseConfig(dsn(t))
	require.NoError(t, err)
	address := fmt.Sprintf("%s:%d", cfg.ConnConfig.Host, cfg.ConnConfig.Port)

	sets := poolAttributeSets(t, reader)
	require.NotEmpty(t, sets, "no pgxpool.* metric was recorded, so nothing below is proven")
	for _, set := range sets {
		name, ok := set.Value("db.client.connection.pool.name")
		require.True(t, ok, "every pool statistic must carry a pool name")
		assert.Equal(t, "horopter", name.AsString())
		for _, kv := range set.ToSlice() {
			assert.NotContains(t, kv.Value.String(), address,
				"no metric attribute may carry the database address")
		}
	}
}
