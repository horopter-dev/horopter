package postgres_test

import (
	"context"
	"fmt"
	"strconv"
	"sync"
	"sync/atomic"
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
	// Each test owns its own tenant, so tests are isolated without truncating shared
	// tables. They must still run sequentially: afterLoadHook and afterIdemLookupHook
	// are package-level vars that the concurrency tests install and clear.
	return s
}

// The tests share a database that nothing truncates, so every identifier a test puts
// into it has to be one no earlier execution used. Otherwise AppendEvent finds the
// stored idempotency key, returns without folding, and the test asserts against rows
// something else wrote — which is a silent vacuous pass for the concurrency test and an
// outright failure for the others.
//
// procToken separates one `go test` process from the next; idSeq separates every call
// within a process. The counter is what makes -count=N honest: a repetition reuses both
// the process and the test name, so a token fixed at either would repeat identifiers
// across iterations and the second iteration would do no work at all.
var (
	procToken = strconv.FormatInt(time.Now().UnixNano(), 36)
	idSeq     atomic.Uint64
)

// uniqueToken returns a token no other call in any execution has produced.
func uniqueToken() string {
	return procToken + "-" + strconv.FormatUint(idSeq.Add(1), 36)
}

// tenant returns a tenant id unique to this call, naming the test for traceability.
func tenant(t *testing.T) string {
	t.Helper()
	return "test-" + t.Name() + "-" + uniqueToken()
}

// eventID returns an event id unique to this call. event.id is a bare primary key with
// no tenant in it, so ids must be unique across every tenant and every execution, not
// merely within the tenant that writes them. suffix names the event for traceability
// and plays no part in uniqueness.
func eventID(t *testing.T, suffix string) string {
	t.Helper()
	return uniqueToken() + "-" + t.Name() + "-" + suffix
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

	evs, _, err := s.ListEvents(ctx, postgres.EventQuery{Tenant: tn})
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

// TestAppendEventNormalizesNilAttributes pins the store's contract at the boundary
// where the NOT NULL constraint actually lives: a nil Attributes map means "no
// attributes," identically to an empty one, and AppendEvent must accept it rather than
// let it reach Postgres as an explicit NULL, which the attributes column rejects
// (DEFAULT only fires when a column is omitted from the INSERT list, not when NULL is
// bound explicitly).
func TestAppendEventNormalizesNilAttributes(t *testing.T) {
	ctx, s, tn := context.Background(), newStore(t), tenant(t)
	at := time.Date(2026, 9, 14, 10, 0, 0, 0, time.UTC)
	ev := mkEvent(tn, eventID(t, "1"), "k1", "dep-7", "deployment.started", at)
	ev.Attributes = nil

	_, created, err := s.AppendEvent(ctx, ev)
	require.NoError(t, err, "a nil Attributes map must not violate the NOT NULL column")
	assert.True(t, created)

	evs, _, err := s.ListEvents(ctx, postgres.EventQuery{Tenant: tn})
	require.NoError(t, err)
	require.Len(t, evs, 1)
	assert.NotNil(t, evs[0].Attributes,
		"the stored event must round-trip as an empty map, not nil")
	assert.Empty(t, evs[0].Attributes)
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

// TestConcurrentAppendsToSameRefDoNotLoseUpdates is the reason applyToProjection takes
// a per-ref advisory lock. Two ingress replicas folding two events onto the same ref
// both read the same prior change and the second write silently discards the first
// event's contribution — no error, nothing in the logs.
//
// Nothing seeds the ref first, and that is deliberate: it is the row-ABSENT path that
// was broken. SELECT ... FOR UPDATE can only lock rows visible to the transaction's
// snapshot, so with no row yet every participant folds from nil and the last
// INSERT ... ON CONFLICT DO UPDATE overwrites the rest wholesale. A seeded ref sends
// every participant down the row-EXISTS path instead, where FOR UPDATE does serialise
// them, and the broken path is never touched. Restoring a seed here would make this
// test pass against the defect it exists to catch.
//
// Two things make the outcome deterministic rather than a matter of timing. A start
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
	err := s.Pool().QueryRow(ctx,
		`SELECT attributes FROM change WHERE tenant = $1 AND ref = $2`, tn, "dep-7").Scan(&attrs)
	require.NoError(t, err)

	for i := 0; i < concurrentAppends; i++ {
		assert.Contains(t, attrs, fmt.Sprintf("attr%d", i),
			"every concurrent event's contribution must survive; a missing key is a lost update")
	}
}

// TestConcurrentDuplicateIdempotencyKeyReturnsTheOriginal covers a CI retry that fires
// while the first attempt is still in flight. The check-then-insert is not atomic, so
// both callers find nothing and both insert; the loser violates uq_event_idem. The spec
// calls duplicates normal traffic, not an error condition, so the loser must get the
// stored event back exactly as the non-concurrent duplicate path returns it.
//
// The hook is a two-party rendezvous, not a sleep: both goroutines block there until
// both have finished their idempotency lookup, so BOTH are guaranteed past the check
// before either inserts, and one of them therefore MUST take the 23505 recovery path.
// A sleep would only make that likely. The rendezvous is safe here — unlike
// afterLoadHook, which must be a sleep — because the lookup is a plain SELECT that
// takes no lock, so neither participant holds anything the other is waiting for.
func TestConcurrentDuplicateIdempotencyKeyReturnsTheOriginal(t *testing.T) {
	ctx, s, tn := context.Background(), newStore(t), tenant(t)
	at := time.Date(2026, 9, 14, 10, 0, 0, 0, time.UTC)
	key := "retry-" + uniqueToken()

	var rendezvous sync.WaitGroup
	rendezvous.Add(2)
	postgres.SetAfterIdemLookupHook(func() { rendezvous.Done(); rendezvous.Wait() })
	t.Cleanup(func() { postgres.SetAfterIdemLookupHook(nil) })

	type result struct {
		ev      core.Event
		created bool
		err     error
	}
	results := make(chan result, 2)
	start := make(chan struct{})
	for i := range 2 {
		ev := mkEvent(tn, eventID(t, fmt.Sprintf("racer%d", i)), key, "dep-7",
			"deployment.started", at)
		go func(ev core.Event) {
			<-start
			got, created, err := s.AppendEvent(ctx, ev)
			results <- result{got, created, err}
		}(ev)
	}
	close(start)

	var (
		got      [2]result
		creates  int
		returned = map[string]bool{}
	)
	for i := range 2 {
		got[i] = <-results
		require.NoError(t, got[i].err,
			"a concurrent retry of the same idempotency key is normal traffic, not an error")
		if got[i].created {
			creates++
		}
		returned[got[i].ev.ID] = true
	}

	assert.Equal(t, 1, creates, "exactly one of the two callers may have created the event")
	assert.Len(t, returned, 1, "both callers must be told about the same stored event")

	evs, _, err := s.ListEvents(ctx, postgres.EventQuery{Tenant: tn})
	require.NoError(t, err)
	require.Len(t, evs, 1, "a concurrent retry must not duplicate the event")
	assert.True(t, returned[evs[0].ID], "the id both callers got must be the id that was stored")
}
