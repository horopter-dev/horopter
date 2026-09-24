package postgres_test

import (
	"context"
	"net/url"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"horopter.dev/horopter/internal/store/postgres"
)

// openWithPoolSize opens a store whose pool holds n connections, so a test can exhaust it by
// holding them.
func openWithPoolSize(t *testing.T, n int) *postgres.Store {
	t.Helper()
	u, err := url.Parse(dsn(t))
	require.NoError(t, err)
	q := u.Query()
	q.Set("pool_max_conns", strconv.Itoa(n))
	u.RawQuery = q.Encode()
	s, err := postgres.Open(context.Background(), u.String())
	require.NoError(t, err)
	t.Cleanup(s.Close)
	return s
}

// waitRecorder collects what a ConnectionWaitObserver is told.
type waitRecorder struct {
	mu    sync.Mutex
	waits []time.Duration
	errs  []error
}

func (r *waitRecorder) observe(wait time.Duration, err error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.waits = append(r.waits, wait)
	r.errs = append(r.errs, err)
}

func (r *waitRecorder) observed() ([]time.Duration, []error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.waits, r.errs
}

func TestConnectionWaitIsReportedToTheObserver(t *testing.T) {
	ctx := context.Background()
	s := openWithPoolSize(t, 1)
	held, err := s.Pool().Acquire(ctx)
	require.NoError(t, err)
	go func() {
		time.Sleep(200 * time.Millisecond)
		held.Release()
	}()
	var rec waitRecorder
	var one int

	err = s.Pool().QueryRow(postgres.WithConnectionWaitObserver(ctx, rec.observe),
		"SELECT 1").Scan(&one)

	require.NoError(t, err)
	waits, errs := rec.observed()
	require.Len(t, waits, 1, "one query acquires one connection")
	assert.GreaterOrEqual(t, waits[0], 150*time.Millisecond,
		"the query waited for the held connection")
	assert.Equal(t, []error{nil}, errs)
}

func TestAFailedAcquisitionIsReportedWithItsWait(t *testing.T) {
	ctx := context.Background()
	s := openWithPoolSize(t, 1)
	held, err := s.Pool().Acquire(ctx)
	require.NoError(t, err)
	t.Cleanup(held.Release)
	var rec waitRecorder
	queryCtx, cancel := context.WithTimeout(
		postgres.WithConnectionWaitObserver(ctx, rec.observe), 100*time.Millisecond)
	defer cancel()
	var one int

	err = s.Pool().QueryRow(queryCtx, "SELECT 1").Scan(&one)

	require.Error(t, err)
	waits, errs := rec.observed()
	require.Len(t, waits, 1)
	assert.GreaterOrEqual(t, waits[0], 80*time.Millisecond)
	assert.ErrorIs(t, errs[0], context.DeadlineExceeded)
}

// TestPoolTracerKeepsOtelpgxSpans checks the wrapper delegates: otelpgx's acquire and
// statement spans must survive it. recordSpans runs before the store opens, because otelpgx
// takes the tracer provider when the pool is built.
func TestPoolTracerKeepsOtelpgxSpans(t *testing.T) {
	rec, restore := recordSpans(t)
	defer restore()
	ctx, s, tn := context.Background(), newStore(t), tenant(t)
	at := time.Date(2026, 9, 17, 10, 0, 0, 0, time.UTC)

	_, _, err := s.QueryChanges(ctx,
		changeQuery(t, tn, at, at.Add(time.Hour), 1, nil))
	require.NoError(t, err)

	names := map[string]bool{}
	for _, span := range rec.Ended() {
		if span.InstrumentationScope().Name == "github.com/exaring/otelpgx" {
			names[span.Name()] = true
		}
	}
	assert.True(t, names["pool.acquire"], "the acquire span must survive; got %v", names)
	delete(names, "pool.acquire")
	assert.NotEmpty(t, names, "the statement span must survive")
}
