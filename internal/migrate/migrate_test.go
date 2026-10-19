package migrate

import (
	"bytes"
	"context"
	"crypto/rand"
	"fmt"
	"log/slog"
	"net/url"
	"os"
	"strconv"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"horopter.dev/horopter/internal/core"
	"horopter.dev/horopter/internal/store/postgres"
	"horopter.dev/horopter/internal/testdb"
)

func dsn(t *testing.T) string {
	t.Helper()
	v := os.Getenv(testdb.DSNVar)
	if v == "" {
		t.Skip("set " + testdb.DSNVar + " to run Postgres tests (docker compose up -d db)")
	}
	return v
}

// The projection is corrupted directly: what is proved is that the flag reaches the rebuild.
func TestRunRebuildsATenantsProjection(t *testing.T) {
	d := dsn(t)
	ctx := context.Background()

	store, err := postgres.Open(ctx, d)
	require.NoError(t, err)
	t.Cleanup(store.Close)

	// The test database is never truncated, and event.id is a global key that must be a ULID.
	token := strconv.FormatInt(time.Now().UnixNano(), 36)
	tn := testdb.NewTenant(t, "migrate-rebuild-"+token)
	at := time.Date(2026, 9, 14, 12, 0, 0, 0, time.UTC)
	_, _, err = store.AppendEvent(ctx, core.Event{
		ID: core.NewULID(time.Now(), rand.Reader), Tenant: tn,
		SchemaVersion: core.CurrentSchemaVersion,
		Source:        "ci", Type: "deployment.started", Subject: "acme/prod", Actor: "ci-bot",
		Title: "from the event log", OccurredAt: at, ReceivedAt: at,
		CorrelationRef: core.RefFromRow("ci/dep-1"), Attributes: map[string]any{},
		Origin: core.OriginProcess, Locus: core.LocusFromRow("ci"),
	})
	require.NoError(t, err)

	title := func() string {
		var got string
		require.NoError(t, store.Pool().QueryRow(ctx,
			`SELECT title FROM change WHERE tenant = $1 AND ref = $2`, tn, "ci/dep-1").Scan(&got))
		return got
	}
	require.Equal(t, "from the event log", title(), "precondition: the fold wrote the title")

	_, err = store.Pool().Exec(ctx,
		`UPDATE change SET title = 'corrupted' WHERE tenant = $1 AND ref = $2`, tn, "ci/dep-1")
	require.NoError(t, err)
	require.Equal(t, "corrupted", title(), "precondition: the projection is now wrong")

	require.NoError(t, Run(ctx, d, tn))

	assert.Equal(t, "from the event log", title(),
		"--rebuild-tenant must replay the event log over the projection; a corrupted row "+
			"surviving means the flag never reached RebuildProjection")
}

// A rebuild on every migrate would take every tenant's write lock on every deploy.
func TestRunWithoutRebuildTenantLeavesTheProjectionAlone(t *testing.T) {
	d := dsn(t)
	ctx := context.Background()

	store, err := postgres.Open(ctx, d)
	require.NoError(t, err)
	t.Cleanup(store.Close)

	token := strconv.FormatInt(time.Now().UnixNano(), 36)
	tn := testdb.NewTenant(t, "migrate-norebuild-"+token)
	at := time.Date(2026, 9, 14, 12, 0, 0, 0, time.UTC)
	_, _, err = store.AppendEvent(ctx, core.Event{
		ID: core.NewULID(time.Now(), rand.Reader), Tenant: tn,
		SchemaVersion: core.CurrentSchemaVersion,
		Source:        "ci", Type: "deployment.started", Subject: "acme/prod", Actor: "ci-bot",
		OccurredAt: at, ReceivedAt: at, CorrelationRef: core.RefFromRow("ci/dep-1"),
		Attributes: map[string]any{}, Origin: core.OriginProcess,
		Locus: core.LocusFromRow("ci"),
	})
	require.NoError(t, err)

	marker := fmt.Sprintf("untouched-%s", token)
	_, err = store.Pool().Exec(ctx,
		`UPDATE change SET title = $3 WHERE tenant = $1 AND ref = $2`, tn, "ci/dep-1", marker)
	require.NoError(t, err)

	require.NoError(t, Run(ctx, d, ""))

	var got string
	require.NoError(t, store.Pool().QueryRow(ctx,
		`SELECT title FROM change WHERE tenant = $1 AND ref = $2`, tn, "ci/dep-1").Scan(&got))
	assert.Equal(t, marker, got,
		"migrating without --rebuild-tenant must not replay anyone's event log")
}

// isolatedSchemaDSN gives a test its own empty schema.
func isolatedSchemaDSN(t *testing.T, base string) string {
	t.Helper()
	ctx := context.Background()

	admin, err := pgxpool.New(ctx, base)
	require.NoError(t, err)
	t.Cleanup(admin.Close)

	schema := fmt.Sprintf("migratelog_%d", time.Now().UnixNano())
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

// captureLog drops the timestamp so two runs that logged the same thing are byte-identical.
func captureLog(t *testing.T) *bytes.Buffer {
	t.Helper()
	previous := slog.Default()
	t.Cleanup(func() { slog.SetDefault(previous) })

	var buf bytes.Buffer
	slog.SetDefault(slog.New(slog.NewJSONHandler(&buf, &slog.HandlerOptions{
		ReplaceAttr: func(_ []string, a slog.Attr) slog.Attr {
			if a.Key == slog.TimeKey {
				return slog.Attr{}
			}
			return a
		},
	})))
	return &buf
}

// Both runs once logged only "schema up to date": the identical output was the bug.
func TestVerifyAndRebuildLogsWhichMigrationsItApplied(t *testing.T) {
	base := dsn(t)
	ctx := context.Background()
	d := isolatedSchemaDSN(t, base)

	buf := captureLog(t)

	first, err := postgres.Open(ctx, d)
	require.NoError(t, err)
	t.Cleanup(first.Close)
	require.NoError(t, verifyAndRebuild(ctx, first, ""))
	built := buf.String()

	buf.Reset()
	second, err := postgres.Open(ctx, d)
	require.NoError(t, err)
	t.Cleanup(second.Close)
	require.NoError(t, verifyAndRebuild(ctx, second, ""))
	alreadyCurrent := buf.String()

	assert.Contains(t, built, "0001_init.sql",
		"the run that built the schema must name the files it applied")
	assert.NotContains(t, alreadyCurrent, "0001_init.sql",
		"a run that applied nothing must not name files, which would claim DDL it did not run")
	assert.NotEqual(t, built, alreadyCurrent,
		"building a schema and finding one already current must not log identically; that "+
			"identity is the defect this test exists for")
}

func TestRunRequiresADSN(t *testing.T) {
	err := Run(context.Background(), "", "")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "DATABASE_URL")
}
