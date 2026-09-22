package postgres_test

import (
	"context"
	"fmt"
	"net/url"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"horopter.dev/horopter/internal/store/postgres"
)

// migration0004Cutoff is the filename freshDatabase stops before, so a test can seed
// old-shape rows and then let Migrate apply exactly 0004 on top of them.
const migration0004Cutoff = "0004_identity.sql"

// freshDatabase creates a database nothing else has ever written to, applies every
// migration up to (not including) 0004, and drops the database in t.Cleanup.
//
// testdb.Pool's shared development database accumulates rows across every test run, so a
// backfill assertion against it would pass whether or not the backfill actually worked.
func freshDatabase(t *testing.T) *pgxpool.Pool {
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
	return pool
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
	pool := freshDatabase(t) // applies 0001-0003 only, then seeds

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

	var eventTenant, changeTenant, ref string
	require.NoError(t, pool.QueryRow(ctx,
		`SELECT tenant FROM event WHERE id = 'e1'`).Scan(&eventTenant))
	require.NoError(t, pool.QueryRow(ctx,
		`SELECT tenant, ref FROM change`).Scan(&changeTenant, &ref))
	assert.Equal(t, tenantID, eventTenant)
	assert.Equal(t, tenantID, changeTenant)
	assert.Equal(t, "legacy/deploy-42", ref)
}

func TestMigration0004PreservesRowCounts(t *testing.T) {
	ctx := context.Background()
	pool := freshDatabase(t)
	seedRows(t, pool, 3, 2) // 3 events, 2 changes

	before := counts(t, pool)
	_, err := postgres.Migrate(ctx, pool)
	require.NoError(t, err)
	assert.Equal(t, before, counts(t, pool), "the backfill must not lose or duplicate rows")
}

func TestMigration0004RejectsALocusContainingASlash(t *testing.T) {
	ctx := context.Background()
	pool := freshDatabase(t)
	_, err := postgres.Migrate(ctx, pool)
	require.NoError(t, err)

	_, err = pool.Exec(ctx, `INSERT INTO credential
		(id, principal_id, type, key_prefix, key_hash, role, source, locus)
		VALUES ('c1', 'p1', 'machine_key', 'pfx', 'hash', 'producer', 'github', 'github/actions')`)
	require.Error(t, err, "a locus containing / makes <locus>/<ref> unsplittable")
	assert.Contains(t, err.Error(), "credential_locus_no_slash")
}
