package postgres_test

import (
	"context"
	"crypto/rand"
	"fmt"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"horopter.dev/horopter/internal/core"
	"horopter.dev/horopter/internal/store/postgres"
)

// Seeds old-shape rows, then lets Migrate apply exactly 0004.
const migration0004Cutoff = "0004_identity.sql"

// A database of its own: the shared dev database would satisfy any backfill assertion.
func freshDatabase(t *testing.T) (*pgxpool.Pool, string) {
	t.Helper()
	ctx := context.Background()
	base := dsn(t)

	admin, err := pgxpool.New(ctx, base)
	require.NoError(t, err)
	t.Cleanup(admin.Close)

	name := fmt.Sprintf("horopter_test_0004_%d", time.Now().UnixNano())
	ident := pgx.Identifier{name}.Sanitize()
	_, err = admin.Exec(ctx, "CREATE DATABASE "+ident)
	require.NoError(t, err)
	t.Cleanup(func() {
		_, err := admin.Exec(context.Background(), "DROP DATABASE IF EXISTS "+ident+" WITH (FORCE)")
		if err != nil {
			t.Logf("fresh database cleanup: drop %s: %v", name, err)
		}
	})

	u, err := url.Parse(base)
	require.NoError(t, err)
	u.Path = "/" + name
	pool, err := pgxpool.New(ctx, u.String())
	require.NoError(t, err)
	t.Cleanup(pool.Close)

	require.NoError(t, postgres.ApplyMigrationsBefore(ctx, pool, migration0004Cutoff))
	return pool, u.String()
}

func mustExec(t *testing.T, pool *pgxpool.Pool, sql string, args ...any) {
	t.Helper()
	_, err := pool.Exec(context.Background(), sql, args...)
	require.NoError(t, err)
}

// Change i takes event i's ref; a ref belonging to no event fails the migration loudly.
func seedRows(t *testing.T, pool *pgxpool.Pool, events, changes int) {
	t.Helper()
	for i := range events {
		mustExec(t, pool, `INSERT INTO event
			(id, tenant, schema_version, source, type, subject, occurred_at, received_at,
			 correlation_ref)
			VALUES ($1, 'acme', 1, 's', 't.started', 'sub', now(), now(), $1)`,
			fmt.Sprintf("e%d", i))
	}
	for i := range changes {
		mustExec(t, pool, `INSERT INTO change
			(tenant, ref, subject, type, status, first_seen_at, last_event_id)
			VALUES ('acme', $1, 'sub', 't', 'in_progress', now(), 'e0')`,
			fmt.Sprintf("e%d", i))
	}
}

type rowCounts struct{ events, changes int }

func counts(t *testing.T, pool *pgxpool.Pool) rowCounts {
	t.Helper()
	ctx := context.Background()
	var c rowCounts
	require.NoError(t, pool.QueryRow(ctx, `SELECT count(*) FROM event`).Scan(&c.events))
	require.NoError(t, pool.QueryRow(ctx, `SELECT count(*) FROM change`).Scan(&c.changes))
	return c
}

func TestMigration0004BackfillsTenantsAndRefs(t *testing.T) {
	ctx := context.Background()
	pool, _ := freshDatabase(t) // applies 0001-0003 only, then seeds

	mustExec(t, pool, `INSERT INTO event
		(id, tenant, schema_version, source, type, subject, occurred_at, received_at,
		 correlation_ref)
		VALUES ('e1', 'acme', 1, 's', 't.started', 'sub', now(), now(), 'deploy-42')`)
	mustExec(t, pool, `INSERT INTO change
		(tenant, ref, subject, type, status, first_seen_at, last_event_id)
		VALUES ('acme', 'deploy-42', 'sub', 't', 'in_progress', now(), 'e1')`)

	_, err := postgres.Migrate(ctx, pool)
	require.NoError(t, err)

	var tenantID, displayName string
	require.NoError(t, pool.QueryRow(ctx,
		`SELECT id, display_name FROM tenant WHERE display_name = 'acme'`).
		Scan(&tenantID, &displayName))
	assert.NotEqual(t, "acme", tenantID, "tenant id must be opaque, not the old string")

	var eventTenant, changeTenant, ref, correlationRef string
	require.NoError(t, pool.QueryRow(ctx,
		`SELECT tenant, correlation_ref FROM event WHERE id = 'e1'`).
		Scan(&eventTenant, &correlationRef))
	require.NoError(t, pool.QueryRow(ctx,
		`SELECT tenant, ref FROM change`).Scan(&changeTenant, &ref))
	assert.Equal(t, tenantID, eventTenant)
	assert.Equal(t, tenantID, changeTenant)
	assert.Equal(t, "legacy/deploy-42", ref)
	assert.Equal(t, "legacy/deploy-42", correlationRef,
		"core.RefFor derives a correlated change's ref from correlation_ref, so prefixing "+
			"only change.ref leaves the projection un-rebuildable")
}

func TestMigration0004PreservesRowCounts(t *testing.T) {
	ctx := context.Background()
	pool, _ := freshDatabase(t)
	seedRows(t, pool, 3, 2) // 3 events, 2 changes

	before := counts(t, pool)
	_, err := postgres.Migrate(ctx, pool)
	require.NoError(t, err)
	assert.Equal(t, before, counts(t, pool), "the backfill must not lose or duplicate rows")
}

// Every ValidateLocus clause: a slash-only CHECK let " github" in through psql.
func TestMigration0004RejectsAMalformedLocus(t *testing.T) {
	ctx := context.Background()
	pool, _ := freshDatabase(t)
	_, err := postgres.Migrate(ctx, pool)
	require.NoError(t, err)

	for name, locus := range map[string]string{
		"a slash makes <locus>/<ref> unsplittable": "github/actions",
		"empty":                             "",
		"leading whitespace":                " github",
		"trailing newline":                  "github\n",
		"over the 64-byte cap":              strings.Repeat("g", 65),
		"the literal reserved legacy locus": "legacy",
	} {
		t.Run(name, func(t *testing.T) {
			_, err := pool.Exec(ctx, `INSERT INTO credential
				(id, principal_id, type, key_prefix, key_hash, role, source, locus)
				VALUES ($1, 'p1', 'machine_key', $1, 'hash', 'producer', 'github', $2)`,
				"c-"+name, locus)
			require.Error(t, err)
			assert.Contains(t, err.Error(), "credential_locus_shape")
		})
	}
}

func TestMigration0006RejectsAMalformedLocusOnAnEvent(t *testing.T) {
	ctx := context.Background()
	pool, _ := freshDatabase(t)
	_, err := postgres.Migrate(ctx, pool)
	require.NoError(t, err)

	mustExec(t, pool, `INSERT INTO tenant (id, display_name) VALUES ('tnt_x', 'x')`)
	for name, locus := range map[string]string{"slash": "a/b", "empty": ""} {
		t.Run(name, func(t *testing.T) {
			_, err := pool.Exec(ctx, `INSERT INTO event
				(id, tenant, schema_version, source, type, subject, occurred_at,
				 received_at, correlation_ref, origin, locus)
				VALUES ($1, 'tnt_x', 2, 'ci', 'config.changed', 'sub', now(), now(),
				        '', 'process', $2)`,
				core.NewULID(legacyAt, rand.Reader), locus)
			require.Error(t, err)
			assert.Contains(t, err.Error(), "event_locus_shape")
		})
	}
}

func TestMigration0004AcceptsALocusWithAnInteriorNewline(t *testing.T) {
	ctx := context.Background()
	pool, _ := freshDatabase(t)
	_, err := postgres.Migrate(ctx, pool)
	require.NoError(t, err)

	// Only the edges are whitespace-checked, as in auth.ValidateLocus.
	mustExec(t, pool, `INSERT INTO principal (id, display_name) VALUES ('p1', 'p1')`)
	mustExec(t, pool, `INSERT INTO credential
		(id, principal_id, type, key_prefix, key_hash, role, source, locus)
		VALUES ('c1', 'p1', 'machine_key', 'pfx', 'hash', 'producer', 'github', $1)`,
		"git\nhub")
}

type legacySeed struct {
	correlatedRef  string
	pointEventID   string
	startedEventID string
}

var legacyAt = time.Date(2026, 9, 14, 10, 0, 0, 0, time.UTC)

// Seeds both shapes: 0004 prefixes correlated refs, 0006 prefixes point refs.
func seedLegacyProjection(t *testing.T, pool *pgxpool.Pool) legacySeed {
	t.Helper()
	seed := legacySeed{
		correlatedRef:  "deploy-42",
		startedEventID: core.NewULID(legacyAt, rand.Reader),
		pointEventID:   core.NewULID(legacyAt.Add(time.Minute), rand.Reader),
	}
	mustExec(t, pool, `INSERT INTO event
		(id, tenant, schema_version, source, type, subject, occurred_at, received_at,
		 correlation_ref)
		VALUES ($1, 'acme', 1, 'ci', 'deployment.started', 'acme/prod', $2, $2, $3)`,
		seed.startedEventID, legacyAt, seed.correlatedRef)
	mustExec(t, pool, `INSERT INTO change
		(tenant, ref, subject, type, title, actor, started_at, first_seen_at, end_at, status,
		 attributes, last_event_id)
		VALUES ('acme', $1, 'acme/prod', 'deployment.started', 'deployment.started', '',
		        $2, $2, NULL, 'in_progress', '{}'::jsonb, $3)`,
		seed.correlatedRef, legacyAt, seed.startedEventID)

	pointAt := legacyAt.Add(time.Minute)
	mustExec(t, pool, `INSERT INTO event
		(id, tenant, schema_version, source, type, subject, occurred_at, received_at,
		 correlation_ref)
		VALUES ($1, 'acme', 1, 'ci', 'config.changed', 'acme/prod', $2, $2, '')`,
		seed.pointEventID, pointAt)
	mustExec(t, pool, `INSERT INTO change
		(tenant, ref, subject, type, title, actor, started_at, first_seen_at, end_at, status,
		 attributes, last_event_id)
		VALUES ('acme', $1, 'acme/prod', 'config.changed', 'config.changed', '',
		        NULL, $2, $2, 'occurred', '{}'::jsonb, $1)`,
		seed.pointEventID, pointAt)
	return seed
}

// As JSON, so the comparison covers every column, generated ones included.
func projectionRows(t *testing.T, pool *pgxpool.Pool) map[string]string {
	t.Helper()
	rows, err := pool.Query(context.Background(),
		`SELECT ref, to_jsonb(c)::text FROM change c ORDER BY ref`)
	require.NoError(t, err)
	defer rows.Close()

	out := map[string]string{}
	for rows.Next() {
		var ref, body string
		require.NoError(t, rows.Scan(&ref, &body))
		out[ref] = body
	}
	require.NoError(t, rows.Err())
	return out
}

// Scoped: packages run in parallel against one shared Postgres.
func projectionRowsForTenant(t *testing.T, pool *pgxpool.Pool, tenantID string) map[string]string {
	t.Helper()
	rows, err := pool.Query(context.Background(),
		`SELECT ref, to_jsonb(c)::text FROM change c WHERE tenant = $1 ORDER BY ref`, tenantID)
	require.NoError(t, err)
	defer rows.Close()

	out := map[string]string{}
	for rows.Next() {
		var ref, body string
		require.NoError(t, rows.Scan(&ref, &body))
		out[ref] = body
	}
	require.NoError(t, rows.Err())
	return out
}

// Pre-0008 standalone terminal events stay stale until a rebuild; this fixture lacks them.
func TestMigrationsLeaveTheProjectionRebuildable(t *testing.T) {
	ctx := context.Background()
	pool, freshDSN := freshDatabase(t)
	seed := seedLegacyProjection(t, pool)

	_, err := postgres.Migrate(ctx, pool)
	require.NoError(t, err)

	var tenantID string
	require.NoError(t, pool.QueryRow(ctx,
		`SELECT id FROM tenant WHERE display_name = 'acme'`).Scan(&tenantID))

	migrated := projectionRows(t, pool)
	require.Len(t, migrated, 2)
	assert.Contains(t, migrated, "legacy/"+seed.correlatedRef,
		"a correlated change's ref is prefixed, and so is the correlation_ref it comes from")
	assert.Contains(t, migrated, "legacy/"+seed.pointEventID,
		"0006 prefixes a point change too, from the locus it backfilled onto the event, "+
			"so there is exactly one ref shape")

	s, err := postgres.OpenNoMigrate(ctx, freshDSN)
	require.NoError(t, err)
	t.Cleanup(s.Close)
	require.NoError(t, s.RebuildProjection(ctx, tenantID))

	assert.Equal(t, migrated, projectionRows(t, pool),
		"a rebuild after the migrations must reproduce the projection exactly — same refs, "+
			"same rows, same contents — or change has stopped being a function of event")
}

// "GitHub" tells COLLATE "C" byte order from en_US sorting.
func TestMigration0007BackfillsTheUnionOfAReferencesSources(t *testing.T) {
	ctx := context.Background()
	pool, _ := freshDatabase(t)

	at := time.Date(2026, 9, 14, 10, 0, 0, 0, time.UTC)
	first := core.NewULID(at, rand.Reader)
	second := core.NewULID(at.Add(time.Minute), rand.Reader)
	third := core.NewULID(at.Add(2*time.Minute), rand.Reader)

	for _, e := range []struct{ id, source string }{
		{first, "buildkite"},
		{second, "github"},
		{third, "GitHub"},
	} {
		mustExec(t, pool, `INSERT INTO event
			(id, tenant, schema_version, source, type, subject, occurred_at, received_at,
			 correlation_ref)
			VALUES ($1, 'acme', 1, $2, 'deployment.started', 'acme/prod', $3, $3, 'dep-1')`,
			e.id, e.source, at)
	}
	mustExec(t, pool, `INSERT INTO change
		(tenant, ref, subject, type, title, actor, started_at, first_seen_at, end_at,
		 status, attributes, last_event_id)
		VALUES ('acme', 'dep-1', 'acme/prod', 'deployment.started', 'deployment.started',
		        '', $1, $1, NULL, 'in_progress', '{}'::jsonb, $2)`,
		at, third)

	_, err := postgres.Migrate(ctx, pool)
	require.NoError(t, err)

	var tenantID string
	require.NoError(t, pool.QueryRow(ctx,
		`SELECT id FROM tenant WHERE display_name = 'acme'`).Scan(&tenantID))

	var sources []string
	var locus string
	require.NoError(t, pool.QueryRow(ctx,
		`SELECT sources, locus FROM change WHERE tenant = $1`, tenantID).Scan(&sources, &locus))
	assert.Equal(t, []string{"GitHub", "buildkite", "github"}, sources,
		"the backfill unions every event on the ref, ordered by byte value to match "+
			"core.Fold — under this database's default collation \"GitHub\" would sort "+
			"after \"buildkite\" instead of before it")
	assert.Equal(t, "legacy", locus,
		"locus is split_part(ref, '/', 1): migration 0004 prefixes this ref to "+
			"'legacy/dep-1', so the first segment is 'legacy'")
}

// foldEventLog names only the columns Fold reads, so a missed column vanishes on replay.
func TestMigration0008PreservesAnAssertionAcrossARebuild(t *testing.T) {
	ctx := context.Background()
	s, tn := newStore(t), tenant(t)
	at := time.Date(2026, 9, 14, 12, 0, 0, 0, time.UTC)

	// The type's suffix says the opposite, so a lost column shows as a point.
	ev := mkEvent(tn, eventID(t, "assert"), "k-assert", "", "config.changed", at)
	ev.Lifecycle = core.LifecycleStarted
	_, _, err := s.AppendEvent(ctx, ev)
	require.NoError(t, err)

	before := projectionRowsForTenant(t, s.Pool(), tn) // ref -> to_jsonb(c), defined in this file
	require.NoError(t, s.RebuildProjection(ctx, tn))

	assert.Equal(t, before, projectionRowsForTenant(t, s.Pool(), tn),
		"a rebuild must reproduce the change exactly; if the assertion is lost, status "+
			"falls back to the type suffix and the change becomes a point")
}

func TestLifecycleVocabularyIsEnforced(t *testing.T) {
	ctx, s, tn := context.Background(), newStore(t), tenant(t)
	at := time.Date(2026, 9, 14, 12, 0, 0, 0, time.UTC)

	for _, lc := range core.Lifecycles {
		t.Run("admits "+string(lc), func(t *testing.T) {
			ev := mkEvent(tn, eventID(t, "ok-"+string(lc)), "k-"+string(lc), "",
				"config.changed", at)
			ev.Lifecycle = lc
			_, _, err := s.AppendEvent(ctx, ev)
			require.NoError(t, err,
				"0008's CHECK must admit every value in core.Lifecycles; %q is in the Go "+
					"vocabulary and not in the SQL one", lc)
		})
	}

	// Validate would refuse this first, so only raw SQL reaches the constraint.
	_, err := s.Pool().Exec(ctx, `INSERT INTO event
		(id, tenant, schema_version, source, type, subject, occurred_at, received_at,
		 correlation_ref, origin, locus, lifecycle)
		VALUES ($1, $2, 2, 'ci', 'config.changed', 's', $3, $3, '', 'process', 'ci', 'bogus')`,
		eventID(t, "bad"), tn, at)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "event_lifecycle_vocabulary")
}

// A subject seen twice tells a minimum from last-wins.
func TestMigration0010BackfillsEachSubjectsEarliestTime(t *testing.T) {
	ctx := context.Background()
	pool, _ := freshDatabase(t)
	at := time.Date(2026, 9, 14, 10, 0, 0, 123456000, time.UTC)

	for _, e := range []struct {
		subject string
		d       time.Duration
	}{{"Deployment/a/x", time.Minute}, {"Service/a/y", 2 * time.Minute}, {"Deployment/a/x", 0}} {
		mustExec(t, pool, `INSERT INTO event
			(id, tenant, schema_version, source, type, subject, occurred_at, received_at,
			 correlation_ref)
			VALUES ($1, 'acme', 1, 'flux', 'x.annotated', $2, $3, $3, 'r-1')`,
			core.NewULID(at.Add(e.d), rand.Reader), e.subject, at.Add(e.d))
	}
	mustExec(t, pool, `INSERT INTO change
		(tenant, ref, subject, type, title, actor, started_at, first_seen_at, end_at,
		 status, attributes, last_event_id)
		VALUES ('acme', 'r-1', 'Deployment/a/x', 'x.annotated', 'x.annotated', '', NULL, $1,
		        NULL, 'in_progress', '{}'::jsonb, '')`, at)

	_, err := postgres.Migrate(ctx, pool)
	require.NoError(t, err)

	var got map[string]int64
	require.NoError(t, pool.QueryRow(ctx,
		`SELECT subject_first_seen FROM change WHERE ref = 'legacy/r-1'`).Scan(&got))
	assert.Equal(t, map[string]int64{
		"Deployment/a/x": at.UnixMicro(),
		"Service/a/y":    at.Add(2 * time.Minute).UnixMicro(),
	}, got)
}

func TestMigration0011TitlesAChangeByItsEarliestTitledStart(t *testing.T) {
	ctx := context.Background()
	pool, _ := freshDatabase(t)
	at := time.Date(2026, 9, 14, 10, 0, 0, 0, time.UTC)

	for _, e := range []struct {
		ref, typ, title string
		d               time.Duration
	}{
		{"r-start", "deploy.started", "Deploy v2.5.0", 0},
		{"r-start", "deploy.completed", "Done", time.Minute},
		{"r-plain", "deploy.progressed", "Halfway", 0},
		{"r-plain", "deploy.completed", "Done", time.Minute},
	} {
		mustExec(t, pool, `INSERT INTO event
			(id, tenant, schema_version, source, type, subject, title, occurred_at, received_at,
			 correlation_ref)
			VALUES ($1, 'acme', 1, 'ci', $2, 'acme/prod', $3, $4, $4, $5)`,
			core.NewULID(at.Add(e.d), rand.Reader), e.typ, e.title, at.Add(e.d), e.ref)
	}
	for _, ref := range []string{"r-start", "r-plain"} {
		mustExec(t, pool, `INSERT INTO change
			(tenant, ref, subject, type, title, actor, started_at, first_seen_at, end_at,
			 status, attributes, last_event_id)
			VALUES ('acme', $1, 'acme/prod', 'deploy.completed', 'Done', '', NULL, $2, $3,
			        'completed', '{}'::jsonb, '')`, ref, at, at.Add(time.Minute))
	}

	_, err := postgres.Migrate(ctx, pool)
	require.NoError(t, err)

	var title string
	var startedAt *time.Time
	require.NoError(t, pool.QueryRow(ctx, `SELECT title, title_started_at FROM change
		WHERE ref = 'legacy/r-start'`).Scan(&title, &startedAt))
	assert.Equal(t, "Deploy v2.5.0", title)
	require.NotNil(t, startedAt)
	assert.True(t, startedAt.Equal(at))

	require.NoError(t, pool.QueryRow(ctx, `SELECT title, title_started_at FROM change
		WHERE ref = 'legacy/r-plain'`).Scan(&title, &startedAt))
	assert.Equal(t, "Done", title, "no titled start: the stored title stands")
	assert.Nil(t, startedAt)
}
