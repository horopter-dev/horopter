package postgres_test

import (
	"context"
	"testing"

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

func TestMigrationsAreIdempotentAndConcurrencySafe(t *testing.T) {
	ctx := context.Background()
	// dsn(t) must run on the test's own goroutine: t.Skip calls runtime.Goexit,
	// which only ends the calling goroutine rather than the test, so calling it
	// from inside the workers below would hang the <-errs loop forever instead
	// of skipping when HOROPTER_TEST_DSN is unset.
	d := dsn(t)

	// Ten simultaneous starts, as N pods rolling out together would produce.
	// Without the advisory lock these race the same DDL.
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
