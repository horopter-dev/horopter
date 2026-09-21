package main

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

	"github.com/horopter-dev/horopter/internal/core"
	"github.com/horopter-dev/horopter/internal/store/postgres"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestRunRebuildsATenantsProjection covers the operator surface for the safety property
// the whole derived-view design rests on: the projection is always rebuildable. Until
// --rebuild-tenant existed there was no way for an operator to exercise it, which made it
// a claim rather than a capability — and migration 0003's truthful backfill leaves rows
// that only a replay can correct.
//
// The projection is corrupted directly rather than by staging a fold bug, because what
// this has to prove is that the flag reaches RebuildProjection at all: a row visibly wrong
// before the run and visibly right after it is the same evidence, without needing a bug to
// exist.
func TestRunRebuildsATenantsProjection(t *testing.T) {
	dsn := os.Getenv("HOROPTER_TEST_DSN")
	if dsn == "" {
		t.Skip("set HOROPTER_TEST_DSN to run migrate tests (docker compose up -d db)")
	}
	ctx := context.Background()

	store, err := postgres.Open(ctx, dsn)
	require.NoError(t, err)
	t.Cleanup(store.Close)

	// The test database is never truncated, so the tenant and the event id must be ones
	// no other test or earlier run has used: event.id is a global primary key, and
	// Validate requires it to be a syntactically valid ULID, so it comes from
	// core.NewULID rather than the token embedded in the tenant id below.
	token := strconv.FormatInt(time.Now().UnixNano(), 36)
	tn := "migrate-rebuild-" + token
	at := time.Date(2026, 9, 14, 12, 0, 0, 0, time.UTC)
	_, _, err = store.AppendEvent(ctx, core.Event{
		ID: core.NewULID(time.Now(), rand.Reader), Tenant: tn, SchemaVersion: 1,
		Source: "ci", Type: "deployment.started", Subject: "acme/prod", Actor: "ci-bot",
		Title: "from the event log", OccurredAt: at, ReceivedAt: at,
		CorrelationRef: "dep-1", Attributes: map[string]any{},
	})
	require.NoError(t, err)

	title := func() string {
		var got string
		require.NoError(t, store.Pool().QueryRow(ctx,
			`SELECT title FROM change WHERE tenant = $1 AND ref = $2`, tn, "dep-1").Scan(&got))
		return got
	}
	require.Equal(t, "from the event log", title(), "precondition: the fold wrote the title")

	_, err = store.Pool().Exec(ctx,
		`UPDATE change SET title = 'corrupted' WHERE tenant = $1 AND ref = $2`, tn, "dep-1")
	require.NoError(t, err)
	require.Equal(t, "corrupted", title(), "precondition: the projection is now wrong")

	require.NoError(t, run(ctx, dsn, tn))

	assert.Equal(t, "from the event log", title(),
		"--rebuild-tenant must replay the event log over the projection; a corrupted row "+
			"surviving means the flag never reached RebuildProjection")
}

// TestRunWithoutRebuildTenantLeavesTheProjectionAlone is the other half: the flag must be
// opt-in. A rebuild that ran on every migrate would take every tenant's write lock on
// every deploy, and this is the only place that would notice.
func TestRunWithoutRebuildTenantLeavesTheProjectionAlone(t *testing.T) {
	dsn := os.Getenv("HOROPTER_TEST_DSN")
	if dsn == "" {
		t.Skip("set HOROPTER_TEST_DSN to run migrate tests (docker compose up -d db)")
	}
	ctx := context.Background()

	store, err := postgres.Open(ctx, dsn)
	require.NoError(t, err)
	t.Cleanup(store.Close)

	token := strconv.FormatInt(time.Now().UnixNano(), 36)
	tn := "migrate-norebuild-" + token
	at := time.Date(2026, 9, 14, 12, 0, 0, 0, time.UTC)
	_, _, err = store.AppendEvent(ctx, core.Event{
		ID: core.NewULID(time.Now(), rand.Reader), Tenant: tn, SchemaVersion: 1,
		Source: "ci", Type: "deployment.started", Subject: "acme/prod", Actor: "ci-bot",
		OccurredAt: at, ReceivedAt: at, CorrelationRef: "dep-1",
		Attributes: map[string]any{},
	})
	require.NoError(t, err)

	marker := fmt.Sprintf("untouched-%s", token)
	_, err = store.Pool().Exec(ctx,
		`UPDATE change SET title = $3 WHERE tenant = $1 AND ref = $2`, tn, "dep-1", marker)
	require.NoError(t, err)

	require.NoError(t, run(ctx, dsn, ""))

	var got string
	require.NoError(t, store.Pool().QueryRow(ctx,
		`SELECT title FROM change WHERE tenant = $1 AND ref = $2`, tn, "dep-1").Scan(&got))
	assert.Equal(t, marker, got,
		"migrating without --rebuild-tenant must not replay anyone's event log")
}

// isolatedSchemaDSN gives a test its own empty schema, so a migration run inside it starts
// from nothing however many times the suite has run against this database before.
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

// captureLog installs a JSON logger writing to the returned buffer, with the timestamp
// dropped so two runs that logged the same thing produce byte-identical output. That is
// what lets the assertion below compare the runs directly rather than by keyword.
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

// TestVerifyAndRebuildLogsWhichMigrationsItApplied pins the difference between a run that
// built the schema and one that found it already current. Both used to log the single line
// "schema up to date", so an operator reading the logs of a migrate Job could not tell
// whether any DDL had run against their database — the two cases were indistinguishable.
//
// The runs are compared to each other, not only to keywords, because equality is the defect
// stated exactly: it is the identical output that was the bug.
func TestVerifyAndRebuildLogsWhichMigrationsItApplied(t *testing.T) {
	base := os.Getenv("HOROPTER_TEST_DSN")
	if base == "" {
		t.Skip("set HOROPTER_TEST_DSN to run migrate tests (docker compose up -d db)")
	}
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
