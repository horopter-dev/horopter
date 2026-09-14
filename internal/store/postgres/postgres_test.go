package postgres_test

import (
	"context"
	"fmt"
	"net/url"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/require"
	"github.com/yo61/horopter/internal/store/postgres"
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
