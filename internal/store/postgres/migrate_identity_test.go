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

// migration0004Cutoff is the filename freshDatabase stops before, so a test can seed
// old-shape rows and then let Migrate apply exactly 0004 on top of them.
const migration0004Cutoff = "0004_identity.sql"

// freshDatabase creates a database nothing else has ever written to, applies every
// migration up to (not including) 0004, and drops the database in t.Cleanup. It returns a
// pool over it and the DSN that reaches it, which is what a test needing a whole
// postgres.Store rather than raw SQL opens.
//
// testdb.Pool's shared development database accumulates rows across every test run, so a
// backfill assertion against it would pass whether or not the backfill actually worked.
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

// seedRows inserts n independent events and m changes under the same old-shape tenant,
// each with its own ref so TestMigration0004PreservesRowCounts can seed more than one of
// each without colliding on change's (tenant, ref) primary key.
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
			fmt.Sprintf("c%d", i))
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

// TestMigration0004RejectsAMalformedLocus pins the second of the locus rule's two
// enforcement points. horoctl's auth.ValidateLocus is the first and gives the readable
// error; this CHECK is what a direct INSERT meets, and the tool's validation passing says
// nothing about whether the constraint does. Every clause ValidateLocus applies is listed,
// because a CHECK covering only the slash let " github" — the whitespace case resolveRef
// exists to protect against — in through psql.
func TestMigration0004RejectsAMalformedLocus(t *testing.T) {
	ctx := context.Background()
	pool, _ := freshDatabase(t)
	_, err := postgres.Migrate(ctx, pool)
	require.NoError(t, err)

	for name, locus := range map[string]string{
		"a slash makes <locus>/<ref> unsplittable": "github/actions",
		"empty":                "",
		"leading whitespace":   " github",
		"trailing newline":     "github\n",
		"over the 64-byte cap": strings.Repeat("g", 65),
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

func TestMigration0004AcceptsALocusWithAnInteriorNewline(t *testing.T) {
	ctx := context.Background()
	pool, _ := freshDatabase(t)
	_, err := postgres.Migrate(ctx, pool)
	require.NoError(t, err)

	// Pins the CHECK to auth.ValidateLocus's rule rather than to a stricter one: only the
	// edges are whitespace-checked, so an interior newline stays legal in both.
	mustExec(t, pool, `INSERT INTO principal (id, display_name) VALUES ('p1', 'p1')`)
	mustExec(t, pool, `INSERT INTO credential
		(id, principal_id, type, key_prefix, key_hash, role, source, locus)
		VALUES ('c1', 'p1', 'machine_key', 'pfx', 'hash', 'producer', 'github', $1)`,
		"git\nhub")
}

// legacySeed is the two change shapes 0004 must treat differently, seeded in the pre-0004
// shape. The change rows are exactly what folding these events produces, so the only thing a
// rebuild can disagree with them about is what 0004 did to the refs.
type legacySeed struct {
	correlatedRef  string
	pointEventID   string
	startedEventID string
}

var legacyAt = time.Date(2026, 9, 14, 10, 0, 0, 0, time.UTC)

// seedLegacyProjection writes one correlated change and one point change under the old
// tenant string, each with the event log that implies it.
//
// Both shapes are seeded deliberately. A correlated change's ref comes from
// event.correlation_ref, so prefixing change.ref alone is invisible until a rebuild; a point
// change's ref is its own event id and its correlation_ref stays empty forever, so it must NOT be
// prefixed. A fixture carrying only correlated events passes with either bug present.
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

// projectionRows renders every change row as JSON, keyed by ref, so a comparison covers every
// column — generated ones included — rather than the handful a test remembered to name.
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

// TestMigration0004LeavesTheProjectionRebuildable is the invariant the whole design rests on
// and the one nothing asserted: change is a derived view of the immutable event log, so it
// must be discardable and replayable at any time — including straight after 0004.
//
// 0004 rewrote change.ref to 'legacy/<ref>' and left event.correlation_ref alone, so
// RebuildProjection deleted every change row and re-inserted it under the UNPREFIXED ref,
// silently undoing the migration. Nothing failed, no row count moved, and
// GET /v1/events?ref=legacy/... matched nothing either way.
func TestMigration0004LeavesTheProjectionRebuildable(t *testing.T) {
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
	assert.Contains(t, migrated, seed.pointEventID,
		"a point change's ref is its own event id, which core.RefFor never prefixes")

	s, err := postgres.OpenNoMigrate(ctx, freshDSN)
	require.NoError(t, err)
	t.Cleanup(s.Close)
	require.NoError(t, s.RebuildProjection(ctx, tenantID))

	assert.Equal(t, migrated, projectionRows(t, pool),
		"a rebuild after 0004 must reproduce the projection exactly — same refs, same rows, "+
			"same contents — or change has stopped being a function of event")
}
