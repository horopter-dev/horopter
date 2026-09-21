package postgres_test

import (
	"context"
	"fmt"
	"net/url"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/horopter-dev/horopter/internal/store/postgres"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
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

// otelpgxScope is the instrumentation scope name both otelpgx.RecordStats and
// otelpgx.NewTracer's metrics register under.
const otelpgxScope = "github.com/exaring/otelpgx"

// otelpgxAttributeSets returns the attribute set of every data point of every metric in the
// otelpgx scope, split into the pgxpool.* pool-statistic metrics (from RecordStats) and every
// other otelpgx metric (the tracer's own db.client.operation.duration and .errors). otelpgx
// records these as int64 sums, an int64 gauge and a float64 histogram.
func otelpgxAttributeSets(
	t *testing.T, reader *sdkmetric.ManualReader,
) (pool, other []attribute.Set) {
	t.Helper()
	var rm metricdata.ResourceMetrics
	require.NoError(t, reader.Collect(context.Background(), &rm))
	for _, scope := range rm.ScopeMetrics {
		if scope.Scope.Name != otelpgxScope {
			continue
		}
		for _, m := range scope.Metrics {
			sets := metricAttributeSets(m.Data)
			if strings.HasPrefix(m.Name, "pgxpool.") {
				pool = append(pool, sets...)
			} else {
				other = append(other, sets...)
			}
		}
	}
	return pool, other
}

// metricAttributeSets returns the attribute set of every data point in one metric's
// aggregation, covering the three shapes otelpgx's own metrics take.
func metricAttributeSets(data metricdata.Aggregation) []attribute.Set {
	switch data := data.(type) {
	case metricdata.Sum[int64]:
		return pointAttributes(data.DataPoints)
	case metricdata.Gauge[int64]:
		return pointAttributes(data.DataPoints)
	case metricdata.Histogram[float64]:
		return histogramAttributes(data.DataPoints)
	default:
		return nil
	}
}

func pointAttributes[N int64 | float64](points []metricdata.DataPoint[N]) []attribute.Set {
	sets := make([]attribute.Set, 0, len(points))
	for _, point := range points {
		sets = append(sets, point.Attributes)
	}
	return sets
}

func histogramAttributes[N int64 | float64](
	points []metricdata.HistogramDataPoint[N],
) []attribute.Set {
	sets := make([]attribute.Set, 0, len(points))
	for _, point := range points {
		sets = append(sets, point.Attributes)
	}
	return sets
}

// attributeKeys returns set's keys, sorted, so two key sets can be compared regardless of
// the order their attributes were recorded in.
func attributeKeys(set attribute.Set) []string {
	keys := make([]string, 0, set.Len())
	for _, kv := range set.ToSlice() {
		keys = append(keys, string(kv.Key))
	}
	sort.Strings(keys)
	return keys
}

// TestStoreRecordsPoolStatisticsUnderAFixedPoolName pins otelpgx's metrics against
// publishing the database's identity on the unauthenticated /metrics listener: the fixed
// pool name override on its pgxpool.* statistics, and the exact attribute key set on every
// other otelpgx metric (its per-operation duration and error counters). A dependency bump
// that added an attribute such as server.address, server.port or db.namespace to either
// family would otherwise publish it with the suite green.
func TestStoreRecordsPoolStatisticsUnderAFixedPoolName(t *testing.T) {
	reader := recordMetrics(t)
	newStore(t) // opening runs migrations, which is what otelpgx's operation metrics need

	pool, other := otelpgxAttributeSets(t, reader)
	require.NotEmpty(t, pool, "no pgxpool.* metric was recorded, so nothing below is proven")
	require.NotEmpty(t, other, "no other otelpgx metric was recorded, so nothing below is proven")

	for _, set := range pool {
		assert.Equal(t, []string{"db.client.connection.pool.name", "db.system.name"},
			attributeKeys(set), "a pgxpool.* metric must carry exactly these two attributes")
		name, _ := set.Value("db.client.connection.pool.name")
		assert.Equal(t, "horopter", name.AsString())
	}
	for _, set := range other {
		assert.Equal(t, []string{"db.system.name", "pgx.operation.type"}, attributeKeys(set),
			"every non-pgxpool otelpgx metric must carry exactly these two attributes")
	}
}

// TestMigrateReportsTheFilesItApplied covers the fact horopter-migrate needs to tell an
// operator what it did. The ledger alone cannot answer it: after the run, a file applied
// just now and a file applied a year ago are both simply present, so a binary that reads
// the ledger afterwards cannot say which of the rows it was responsible for.
//
// The expectation is the ledger's own contents rather than a hardcoded list of filenames,
// so adding a migration does not require editing this test to keep it passing — what is
// asserted is the invariant (a fresh schema reports every file it ran) and not the number
// of files that happen to exist today.
func TestMigrateReportsTheFilesItApplied(t *testing.T) {
	ctx := context.Background()

	pool, err := pgxpool.New(ctx, isolatedSchemaDSN(t, dsn(t)))
	require.NoError(t, err)
	t.Cleanup(pool.Close)

	applied, err := postgres.Migrate(ctx, pool)
	require.NoError(t, err)

	rows, err := pool.Query(ctx, `SELECT filename FROM applied_migration ORDER BY filename`)
	require.NoError(t, err)
	defer rows.Close()
	ledger := []string{}
	for rows.Next() {
		var name string
		require.NoError(t, rows.Scan(&name))
		ledger = append(ledger, name)
	}
	require.NoError(t, rows.Err())

	require.NotEmpty(t, ledger, "the fixture is wrong if the schema migrated to nothing")
	assert.Equal(t, ledger, applied,
		"a fresh schema must report every migration it ran, in the order it ran them")

	again, err := postgres.Migrate(ctx, pool)
	require.NoError(t, err)
	assert.Empty(t, again,
		"a second run applies nothing, so it must report nothing; reporting the ledger "+
			"again would tell an operator DDL ran when none did")
}
