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
//
// Change i takes event i's correlation_ref as its own, which is what a change row IS — the
// projection of the events sharing that ref. A ref belonging to no event was never a state
// the system could reach, and since 0006 it is one the schema refuses: change_ref_has_locus
// requires every ref to carry a locus, and only a ref a migration recognises as correlated
// (0004) or as a point (0006) is given one. Seeding more changes than events therefore
// fails loudly in the migration rather than silently counting rows that mean nothing.
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

// TestMigration0006RejectsAMalformedLocusOnAnEvent pins the backstop half of the locus
// rule, as TestMigration0004RejectsAMalformedLocus does for the credential. core.Event's
// Validate is the authority and runs first; this is what stops a direct INSERT, by some
// later path that skips it, storing a locus that would make <locus>/<ref> unsplittable.
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

	// Pins the CHECK to auth.ValidateLocus's rule rather than to a stricter one: only the
	// edges are whitespace-checked, so an interior newline stays legal in both.
	mustExec(t, pool, `INSERT INTO principal (id, display_name) VALUES ('p1', 'p1')`)
	mustExec(t, pool, `INSERT INTO credential
		(id, principal_id, type, key_prefix, key_hash, role, source, locus)
		VALUES ('c1', 'p1', 'machine_key', 'pfx', 'hash', 'producer', 'github', $1)`,
		"git\nhub")
}

// legacySeed is the two change shapes the migrations must treat differently, seeded in the
// pre-0004 shape. The change rows are exactly what folding these events produces, so the only
// thing a rebuild can disagree with them about is what 0004 and 0006 did to the refs.
type legacySeed struct {
	correlatedRef  string
	pointEventID   string
	startedEventID string
}

var legacyAt = time.Date(2026, 9, 14, 10, 0, 0, 0, time.UTC)

// seedLegacyProjection writes one correlated change and one point change under the old
// tenant string, each with the event log that implies it.
//
// Both shapes are seeded deliberately, and each is prefixed from a different source. A
// correlated change's ref comes from event.correlation_ref, which 0004 prefixes alongside it,
// so prefixing change.ref alone is invisible until a rebuild. A point change's correlation_ref
// stays empty forever, so there is nothing alongside to prefix: 0006 prefixes its ref from the
// locus it backfilled onto the event, which is the same value core.RefFor composes a point ref
// from. Both end up <locus>/<something>.
//
// A fixture carrying only correlated events cannot tell any of that apart. It passes with 0004's
// bug present, and with either of 0006's two failure modes — an unbackfilled event.locus, and a
// foldEventLog that does not select it.
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

// projectionRowsForTenant is projectionRows scoped to one tenant. projectionRows' other
// callers each own a private freshDatabase; TestMigration0008PreservesAnAssertionAcrossARebuild
// runs against the shared dev database instead, where an unscoped read can catch a row an
// unrelated, concurrently running package's test wrote between the "before" and "after"
// snapshots — go test ./... runs packages in parallel, and every package's tests share this
// one Postgres instance.
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

// TestMigrationsLeaveTheProjectionRebuildable is the invariant the whole design rests on
// and the one nothing asserted until 0004 broke it: change is a derived view of the
// immutable event log, so it must be discardable and replayable at any time — including
// straight after a migration that rewrites refs.
//
// 0004 rewrote change.ref to 'legacy/<ref>' and left event.correlation_ref alone, so
// RebuildProjection deleted every change row and re-inserted it under the UNPREFIXED ref,
// silently undoing the migration. 0006 can fail the same way twice over: by not backfilling
// event.locus, and by not selecting it in foldEventLog.
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

// TestMigration0007BackfillsTheUnionOfAReferencesSources pins the backfill to the same rule
// core.Fold applies, which is what makes a rebuild straight after the migration a no-op.
//
// Two events on one ref with different sources is exactly the fixture a union backfill
// needs: seedLegacyProjection has one event per change, so it would pass under a backfill
// that took any ONE event's source. It does not, on its own, kill the
// `e.correlation_ref = c.ref` mutant — under that mutation a point change's
// correlation_ref stays empty, matching no ref, so its sources stay at the column's empty
// default while a rebuild fills it in, whereas a correlated change can come out right by
// luck (`UPDATE ... FROM` with several matching rows picks one arbitrarily). That mutant is
// what seedLegacyProjection's point-change fixture kills, in
// TestMigrationsLeaveTheProjectionRebuildable — the two fixtures are not redundant.
//
// The third source, "GitHub", is what tells COLLATE "C" apart from this database's default
// collation: en_US.utf8 sorts "GitHub" after "buildkite", but byte order — what
// core.Fold's slices.Sort uses, and what the migration must match — sorts capital letters
// before lowercase ones, putting it first. A fixture of all-lowercase sources cannot
// distinguish the two orderings.
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

// TestMigration0008PreservesAnAssertionAcrossARebuild is the invariant for the lifecycle
// column, and it fails for a reason no unit test can reach: foldEventLog names only the
// columns Fold reads, so a lifecycle omitted there folds correctly live and vanishes on
// replay. That is 0006's second failure mode, and this is the shape that catches it.
func TestMigration0008PreservesAnAssertionAcrossARebuild(t *testing.T) {
	ctx := context.Background()
	s, tn := newStore(t), tenant(t)
	at := time.Date(2026, 9, 14, 12, 0, 0, 0, time.UTC)

	// An explicit assertion on a type whose suffix says the OPPOSITE. If the column is
	// lost, the convention takes over and the change folds as a point — so this fixture
	// discriminates the column from the fallback, which a .started type could not.
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

// TestLifecycleVocabularyIsEnforced pins core.Lifecycles to migration 0008's CHECK, in the
// shape TestChangeStatusVocabularyIsEnforced uses for Status — which exists because a
// seventh Status was added without a migration, compiled, passed, and failed at runtime on
// the first insert.
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

	// The other direction, through raw SQL: Validate would refuse this before the store
	// saw it, so only a direct insert reaches the constraint.
	_, err := s.Pool().Exec(ctx, `INSERT INTO event
		(id, tenant, schema_version, source, type, subject, occurred_at, received_at,
		 correlation_ref, origin, locus, lifecycle)
		VALUES ($1, $2, 2, 'ci', 'config.changed', 's', $3, $3, '', 'process', 'ci', 'bogus')`,
		eventID(t, "bad"), tn, at)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "event_lifecycle_vocabulary")
}
