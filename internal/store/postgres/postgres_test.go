package postgres_test

import (
	"context"
	"fmt"
	"net/url"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/otel/attribute"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
	"horopter.dev/horopter/internal/store/postgres"
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

// A never-migrated schema of its own, so the concurrent DDL genuinely races.
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
	// On the test goroutine: t.Skip from a worker would hang the loop.
	base := dsn(t)
	d := isolatedSchemaDSN(t, base)

	// Ten starts against an unmigrated schema, like pods rolling out together.
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

// 0002's named CHECK cannot be added twice, so a second Open succeeding proves the ledger.
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
		// Arbitrary body: ADD CONSTRAINT fails on the duplicate name alone.
		_, err := second.Pool().Exec(ctx, `
			ALTER TABLE change ADD CONSTRAINT change_status_vocabulary CHECK (true)`)
		require.Error(t, err,
			"if this statement were idempotent the ledger test above would prove nothing")
	})
}

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

const otelpgxScope = "github.com/exaring/otelpgx"

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

func attributeKeys(set attribute.Set) []string {
	keys := make([]string, 0, set.Len())
	for _, kv := range set.ToSlice() {
		keys = append(keys, string(kv.Key))
	}
	sort.Strings(keys)
	return keys
}

// A bump adding server.address or db.namespace must fail: /metrics is unauthenticated.
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

// Expects the ledger's own contents, so a new migration needs no edit here.
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
