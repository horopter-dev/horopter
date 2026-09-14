package postgres_test

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/yo61/horopter/internal/core"
	"github.com/yo61/horopter/internal/store/postgres"
)

func newStore(t *testing.T) *postgres.Store {
	t.Helper()
	ctx := context.Background()
	s, err := postgres.Open(ctx, dsn(t))
	require.NoError(t, err)
	t.Cleanup(s.Close)
	// Each test owns its own tenant, so tests are isolated without truncating
	// shared tables and can run in parallel.
	return s
}

// runID distinguishes one `go test` process from the next. The tests share a database
// that nothing truncates, so identifiers scoped only to a test name are reused by the
// following run: AppendEvent then finds the stored idempotency key, returns without
// folding, and the test asserts against rows an earlier run wrote. Every identifier a
// test puts in the database therefore carries this token.
var runID = fmt.Sprintf("%d", time.Now().UnixNano())

// tenant returns a tenant id unique to this test and this run, keeping tests isolated
// from one another and from every previous run.
func tenant(t *testing.T) string {
	t.Helper()
	return "test-" + t.Name() + "-" + runID
}

// eventID returns an event id unique to this test and this run. event.id is a bare
// primary key with no tenant in it, so a per-tenant identifier is not enough to keep
// two tests, or two runs of one test, from colliding on it.
func eventID(t *testing.T, suffix string) string {
	t.Helper()
	return runID + "-" + t.Name() + "-" + suffix
}

func mkEvent(tn, id, idem, corr, typ string, at time.Time) core.Event {
	return core.Event{
		ID: id, Tenant: tn, IdempotencyKey: idem, SchemaVersion: 1,
		Source: "ci", Type: typ, Subject: "acme/prod", Actor: "ci-bot",
		OccurredAt: at, ReceivedAt: at, CorrelationRef: corr,
		Attributes: map[string]any{"service.version": "2.5.0"},
	}
}

func TestAppendEventStoresAndProjects(t *testing.T) {
	ctx, s, tn := context.Background(), newStore(t), tenant(t)
	at := time.Date(2026, 9, 14, 10, 0, 0, 0, time.UTC)

	_, created, err := s.AppendEvent(ctx,
		mkEvent(tn, eventID(t, "1"), "k1", "dep-7", "deployment.started", at))
	require.NoError(t, err)
	assert.True(t, created)

	var ref, status string
	var endAt *time.Time
	err = s.Pool().QueryRow(ctx,
		`SELECT ref, status, end_at FROM change WHERE tenant = $1 AND ref = $2`,
		tn, "dep-7").Scan(&ref, &status, &endAt)
	require.NoError(t, err)
	assert.Equal(t, "dep-7", ref)
	assert.Equal(t, "in_progress", status)
	assert.Nil(t, endAt, "a started-but-unfinished change must have a NULL end")
}

func TestAppendEventIsIdempotent(t *testing.T) {
	ctx, s, tn := context.Background(), newStore(t), tenant(t)
	at := time.Date(2026, 9, 14, 10, 0, 0, 0, time.UTC)
	e := mkEvent(tn, eventID(t, "1"), "build-42", "dep-7", "deployment.started", at)

	first, created, err := s.AppendEvent(ctx, e)
	require.NoError(t, err)
	require.True(t, created)

	dup := e
	dup.ID = eventID(t, "2") // a retry generates a new id but reuses the key
	second, created, err := s.AppendEvent(ctx, dup)
	require.NoError(t, err)
	assert.False(t, created, "a duplicate idempotency key must not create a second event")
	assert.Equal(t, first.ID, second.ID, "the originally stored event must be returned")

	evs, err := s.ListEvents(ctx, postgres.EventQuery{Tenant: tn})
	require.NoError(t, err)
	assert.Len(t, evs, 1, "CI retries must not duplicate events")
}

func TestAppendEventRejectsInvalid(t *testing.T) {
	ctx, s, tn := context.Background(), newStore(t), tenant(t)
	bad := mkEvent(tn, eventID(t, "1"), "k1", "dep-7", "deployment.started", time.Now())
	bad.Subject = ""

	_, _, err := s.AppendEvent(ctx, bad)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "subject")
}

// concurrentAppends is how many goroutines fold onto the same ref at once. Two would
// prove the point; a handful makes the failure message informative. It must not exceed
// four, pgxpool's floor for MaxConns: every participant holds a connection for the whole
// of its transaction, so a larger number can queue on the pool instead of overlapping.
const concurrentAppends = 4

// projectionWriteDelay is how long afterLoadHook holds each transaction between its
// projection read and its write. It only has to outlast the time a released goroutine
// needs to reach that read — single-digit milliseconds against a local database — so
// this is headroom, not a tuned value.
const projectionWriteDelay = 200 * time.Millisecond

// TestConcurrentAppendsToSameRefDoNotLoseUpdates is the reason the projection upsert
// takes a row lock. Without SELECT ... FOR UPDATE, two ingress replicas folding two
// events onto the same ref both read the same prior change and the second write
// silently discards the first event's contribution — no error, nothing in the logs.
//
// Two things make that outcome deterministic rather than a matter of timing. A start
// channel releases every goroutine at once, so they overlap instead of serialising in
// spawn order; and afterLoadHook holds each transaction between its read and its write
// long enough that all of them have read before any of them writes.
//
// The hook has to be a sleep rather than a rendezvous between the participants: with
// the lock in place only one transaction reaches the hook at a time, so a hook that
// waited for the others would deadlock in exactly the case that must pass.
func TestConcurrentAppendsToSameRefDoNotLoseUpdates(t *testing.T) {
	ctx, s, tn := context.Background(), newStore(t), tenant(t)
	at := time.Date(2026, 9, 14, 10, 0, 0, 0, time.UTC)

	_, _, err := s.AppendEvent(ctx,
		mkEvent(tn, eventID(t, "seed"), "k0", "dep-7", "deployment.started", at))
	require.NoError(t, err)

	postgres.SetAfterLoadHook(func() { time.Sleep(projectionWriteDelay) })
	t.Cleanup(func() { postgres.SetAfterLoadHook(nil) })

	// Every event is built before any goroutine is released, so a released goroutine has
	// nothing left to do but call AppendEvent.
	start := make(chan struct{})
	var wg sync.WaitGroup
	for i := 0; i < concurrentAppends; i++ {
		ev := mkEvent(tn, eventID(t, fmt.Sprintf("%02d", i)), fmt.Sprintf("k%d", i+1),
			"dep-7", "deployment.annotated", at.Add(time.Duration(i)*time.Second))
		ev.Attributes = map[string]any{fmt.Sprintf("attr%d", i): "set"}
		wg.Add(1)
		go func(ev core.Event) {
			defer wg.Done()
			<-start
			_, _, err := s.AppendEvent(ctx, ev)
			assert.NoError(t, err)
		}(ev)
	}
	close(start)
	wg.Wait()

	var attrs map[string]any
	err = s.Pool().QueryRow(ctx,
		`SELECT attributes FROM change WHERE tenant = $1 AND ref = $2`, tn, "dep-7").Scan(&attrs)
	require.NoError(t, err)

	for i := 0; i < concurrentAppends; i++ {
		assert.Contains(t, attrs, fmt.Sprintf("attr%d", i),
			"every concurrent event's contribution must survive; a missing key is a lost update")
	}
}
