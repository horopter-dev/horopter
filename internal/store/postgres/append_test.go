package postgres_test

import (
	"context"
	"crypto/rand"
	"fmt"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/oklog/ulid/v2"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/yo61/horopter/internal/core"
	"github.com/yo61/horopter/internal/store/postgres"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
)

func newStore(t *testing.T) *postgres.Store {
	t.Helper()
	ctx := context.Background()
	s, err := postgres.Open(ctx, dsn(t))
	require.NoError(t, err)
	t.Cleanup(s.Close)
	// Each test owns its own tenant, so tests are isolated without truncating shared
	// tables. They must still run sequentially: afterLoadHook, afterIdemLookupHook and
	// afterDeleteHook are package-level vars that the concurrency tests install and clear.
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

// monotonicEntropy makes successive eventID calls sort in call order even when two land
// in the same millisecond, which core.NewULID's production entropy (plain crypto/rand)
// does not guarantee — see RebuildProjection's own doc comment on why that gap matters.
// rebuild_test.go's projection-equality test depends on id order matching call order: a
// tie broken by pure random entropy could let two same-millisecond events swap places
// under RebuildProjection's id-ordered replay, which folds in a different order than the
// live appends that built "before" did.
//
// It is package-level, not per-call, because monotonicity is a property of a SEQUENCE of
// reads sharing one accumulator, and it is not safe for concurrent use — which is fine
// here, since eventID is only ever called from a test's own goroutine, before any worker
// goroutines the test spawns are released (see TestConcurrentAppendsToSameRefDoNotLoseUpdates
// and TestConcurrentDuplicateIdempotencyKeyReturnsTheOriginal, both of which build every
// event before starting a single goroutine).
var monotonicEntropy = ulid.Monotonic(rand.Reader, 0)

// eventID returns a fresh, valid ULID for use as an event id. event.id is a bare
// primary key with no tenant in it, so ids must be unique across every tenant and every
// execution, not merely within the tenant that writes them; monotonicEntropy's entropy
// gives that guarantee the same way core.NewULID's does in production, plus the call
// ordering guarantee production does not need and this package's tests do.
//
// core.Event.Validate rejects anything that is not a syntactically valid ULID, which
// rules out embedding suffix in the id text the way this helper used to; suffix is
// logged instead so a failing test's -v output still names the call that built the id.
func eventID(t *testing.T, suffix string) string {
	t.Helper()
	id := core.NewULID(time.Now(), monotonicEntropy)
	t.Logf("eventID %q -> %s", suffix, id)
	return id
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
// both callers find nothing and both insert; the loser violates uq_event_idem. Duplicates
// are normal traffic, not an error condition, so the loser must get the
// stored event back exactly as the non-concurrent duplicate path returns it.
//
// The hook is a two-party rendezvous, not a sleep: both goroutines block there until
// both have finished their idempotency lookup, so BOTH are guaranteed past the check
// before either inserts, and one of them therefore MUST take the 23505 recovery path.
// A sleep would only make that likely. The rendezvous is safe here — unlike
// afterLoadHook, which must be a sleep — because the lookup is a plain SELECT that
// takes no lock, so neither participant holds anything the other is waiting for.
//
// hookCalls is what stops the test passing vacuously. Delete the hook's call site in the
// store and the rendezvous never happens: the two goroutines usually serialise, the loser
// finds the row at the ordinary lookup, and every assertion below still holds — while the
// recovery path the test is named for was never entered. Exactly two calls is proof both
// goroutines really did get past the check before either inserted.
func TestConcurrentDuplicateIdempotencyKeyReturnsTheOriginal(t *testing.T) {
	rec, restore := recordSpans(t)
	defer restore()
	reader := recordMetrics(t)

	ctx, s, tn := context.Background(), newStore(t), tenant(t)
	at := time.Date(2026, 9, 14, 10, 0, 0, 0, time.UTC)
	key := "retry-" + uniqueToken()

	var (
		hookCalls  atomic.Int64
		rendezvous sync.WaitGroup
	)
	rendezvous.Add(2)
	postgres.SetAfterIdemLookupHook(func() {
		hookCalls.Add(1)
		rendezvous.Done()
		rendezvous.Wait()
	})
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

	assert.Equal(t, int64(2), hookCalls.Load(),
		"both callers must have passed the idempotency check before either inserted; "+
			"fewer calls means they serialised and the 23505 recovery path this test is "+
			"named for was never exercised")

	paths := spanAttrValues(rec, "AppendEvent", "horopter.append.dedupe_path")
	assert.Contains(t, paths, "unique_violation",
		"one of the two racers must lose on uq_event_idem and take the recovery path; this "+
			"is the only test in the suite that reaches it")

	assert.Equal(t,
		map[string]int64{appendCreated: 1, appendDeduplicatedByUniqueViolation: 1},
		appendCounts(t, reader),
		"the recovery path must be counted as a deduplication, not a failure")
}

// recordSpans installs a recording provider and returns it with a restore function.
//
// The restore is best-effort and deliberately named for what it attempts rather than what
// it guarantees: OTel's global provider delegates ONE WAY, so a provider installed once
// cannot be fully uninstalled within a process, and a later test can still record into a
// recorder installed here. That is why every assertion below filters by span name and
// attribute key — never write one that depends on the recorder being otherwise empty.
func recordSpans(t *testing.T) (*tracetest.SpanRecorder, func()) {
	t.Helper()
	rec := tracetest.NewSpanRecorder()
	prev := otel.GetTracerProvider()
	otel.SetTracerProvider(sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(rec)))
	return rec, func() { otel.SetTracerProvider(prev) }
}

// spanAttrValues returns key's value from every ended span named spanName, in the order
// the spans ended. A span lacking the attribute is SKIPPED rather than contributing an
// empty entry, so an exact-slice assertion reads "these spans carried it" instead of
// "these spans existed". Pass "" as spanName to match every span.
func spanAttrValues(rec *tracetest.SpanRecorder, spanName, key string) []string {
	var out []string
	for _, span := range rec.Ended() {
		if spanName != "" && span.Name() != spanName {
			continue
		}
		for _, kv := range span.Attributes() {
			if string(kv.Key) == key {
				out = append(out, kv.Value.String())
			}
		}
	}
	return out
}

// recordMetrics installs a meter provider with a manual reader for the rest of the test.
//
// Call it BEFORE newStore. A store creates its append counter from the global meter provider
// when it opens, so a store opened first keeps counting into whatever provider was global
// then — the same one-way delegation recordSpans documents.
func recordMetrics(t *testing.T) *sdkmetric.ManualReader {
	t.Helper()
	reader := sdkmetric.NewManualReader()
	provider := sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader))
	prev := otel.GetMeterProvider()
	otel.SetMeterProvider(provider)
	t.Cleanup(func() {
		otel.SetMeterProvider(prev)
		_ = provider.Shutdown(context.Background())
	})
	return reader
}

// appendCounts returns horopter.event.appends' count for each attribute set, keyed by the
// set's encoding: "key=value" pairs in key order, joined by commas.
func appendCounts(t *testing.T, reader *sdkmetric.ManualReader) map[string]int64 {
	t.Helper()
	var rm metricdata.ResourceMetrics
	require.NoError(t, reader.Collect(context.Background(), &rm))
	counts := map[string]int64{}
	for _, scope := range rm.ScopeMetrics {
		for _, m := range scope.Metrics {
			if m.Name != "horopter.event.appends" {
				continue
			}
			sum, ok := m.Data.(metricdata.Sum[int64])
			require.True(t, ok, "horopter.event.appends must be an int64 counter, got %T", m.Data)
			for _, point := range sum.DataPoints {
				counts[point.Attributes.Encoded(attribute.DefaultEncoder())] = point.Value
			}
		}
	}
	return counts
}

// The attribute sets horopter.event.appends records, written out by hand: every key the
// counter may carry, and nothing that identifies a caller or an entity. An assertion against
// these fails if a key is added — a tenant, say — which is what makes them the cardinality
// guard as well.
const (
	appendCreated = "horopter.append.dedupe_path=none,horopter.append.outcome=created," +
		"horopter.fold.out_of_order=false"
	appendCreatedOutOfOrder = "horopter.append.dedupe_path=none," +
		"horopter.append.outcome=created,horopter.fold.out_of_order=true"
	appendDeduplicatedByLookup = "horopter.append.dedupe_path=lookup," +
		"horopter.append.outcome=deduplicated,horopter.fold.out_of_order=false"
	appendDeduplicatedByUniqueViolation = "horopter.append.dedupe_path=unique_violation," +
		"horopter.append.outcome=deduplicated,horopter.fold.out_of_order=false"
	appendFailedInvalid = "horopter.append.dedupe_path=none,horopter.append.outcome=failed," +
		"horopter.error.kind=invalid_event,horopter.fold.out_of_order=false"
	appendFailedTransient = "horopter.append.dedupe_path=none," +
		"horopter.append.outcome=failed,horopter.error.kind=transient," +
		"horopter.fold.out_of_order=false"
)

func TestAppendCountsCreatedAndDeduplicatedEvents(t *testing.T) {
	reader := recordMetrics(t)
	ctx, s, tn := context.Background(), newStore(t), tenant(t)
	at := time.Date(2026, 9, 16, 10, 0, 0, 0, time.UTC)

	first := mkEvent(tn, eventID(t, "a"), "dup-key", "dep-1", "deployment.started", at)
	_, created, err := s.AppendEvent(ctx, first)
	require.NoError(t, err)
	require.True(t, created)
	retry := mkEvent(tn, eventID(t, "b"), "dup-key", "dep-1", "deployment.started", at)
	_, created, err = s.AppendEvent(ctx, retry)
	require.NoError(t, err)
	require.False(t, created)

	assert.Equal(t, map[string]int64{appendCreated: 1, appendDeduplicatedByLookup: 1},
		appendCounts(t, reader))
}

// TestAppendCountsOutOfOrderEvents measures the rate the out-of-order decision says has
// never been observed. Ids are minted fresh and ordered, as TestAppendRecordsAnOutOfOrderFold
// explains, because this suite never truncates its database.
func TestAppendCountsOutOfOrderEvents(t *testing.T) {
	reader := recordMetrics(t)
	ctx, s, tn := context.Background(), newStore(t), tenant(t)
	at := time.Date(2026, 9, 16, 10, 0, 0, 0, time.UTC)

	a, b := eventID(t, "x"), eventID(t, "y")
	if a > b {
		a, b = b, a
	}
	_, _, err := s.AppendEvent(ctx, mkEvent(tn, b, "k1", "dep-2", "deployment.started", at))
	require.NoError(t, err)
	_, _, err = s.AppendEvent(ctx, mkEvent(tn, a, "k2", "dep-2", "deployment.completed", at))
	require.NoError(t, err)

	assert.Equal(t, map[string]int64{appendCreated: 1, appendCreatedOutOfOrder: 1},
		appendCounts(t, reader))
}

func TestAppendCountsAnInvalidEvent(t *testing.T) {
	reader := recordMetrics(t)
	ctx, s, tn := context.Background(), newStore(t), tenant(t)
	at := time.Date(2026, 9, 16, 10, 0, 0, 0, time.UTC)

	bad := mkEvent(tn, eventID(t, "bad"), "k1", "dep-3", "deployment.started", at)
	bad.Subject = ""
	_, _, err := s.AppendEvent(ctx, bad)
	require.Error(t, err)

	assert.Equal(t, map[string]int64{appendFailedInvalid: 1}, appendCounts(t, reader))
}

// TestAppendCountsATransientFailure closes the pool, which is a real outage rather than a
// faked error: Begin fails the way it fails when Postgres is gone. pgxpool.Close is guarded
// by a sync.Once, so newStore's own cleanup close is still safe.
func TestAppendCountsATransientFailure(t *testing.T) {
	reader := recordMetrics(t)
	ctx, s, tn := context.Background(), newStore(t), tenant(t)
	at := time.Date(2026, 9, 16, 10, 0, 0, 0, time.UTC)
	s.Close()

	_, _, err := s.AppendEvent(ctx,
		mkEvent(tn, eventID(t, "down"), "k1", "dep-4", "deployment.started", at))
	require.Error(t, err)

	assert.Equal(t, map[string]int64{appendFailedTransient: 1}, appendCounts(t, reader))
}

func TestAppendRecordsTheDedupePath(t *testing.T) {
	rec, restore := recordSpans(t)
	defer restore()

	ctx, s, tn := context.Background(), newStore(t), tenant(t)
	at := time.Date(2026, 9, 15, 10, 0, 0, 0, time.UTC)
	ev := mkEvent(tn, eventID(t, "a"), "dup-key", "dep-1", "deployment.started", at)

	_, created, err := s.AppendEvent(ctx, ev)
	require.NoError(t, err)
	require.True(t, created)

	second := mkEvent(tn, eventID(t, "b"), "dup-key", "dep-1", "deployment.started", at)
	_, created, err = s.AppendEvent(ctx, second)
	require.NoError(t, err)
	require.False(t, created)

	paths := spanAttrValues(rec, "AppendEvent", "horopter.append.dedupe_path")
	assert.Equal(t, []string{"none", "lookup"}, paths,
		"a first append takes no dedupe path; a retry is caught by the lookup")
}

func TestAppendRecordsAnOutOfOrderFold(t *testing.T) {
	rec, restore := recordSpans(t)
	defer restore()

	ctx, s, tn := context.Background(), newStore(t), tenant(t)
	at := time.Date(2026, 9, 15, 10, 0, 0, 0, time.UTC)

	// Append a high id first, then a lower one on the same ref: commit order and id
	// order now disagree, which is the rebuild-divergence condition.
	// Mint two fresh ids and order them, rather than hardcoding literals: event.id is
	// the PRIMARY KEY and this suite never truncates its database, so a literal id
	// passes once and then violates event_pkey forever. That violation is not on
	// uq_event_idem, so recoverConcurrentDuplicate does not absorb it.
	a, b := eventID(t, "x"), eventID(t, "y")
	if a > b {
		a, b = b, a
	}
	high := mkEvent(tn, b, "k1", "dep-2", "deployment.started", at)
	low := mkEvent(tn, a, "k2", "dep-2", "deployment.completed", at)
	_, _, err := s.AppendEvent(ctx, high)
	require.NoError(t, err)
	_, _, err = s.AppendEvent(ctx, low)
	require.NoError(t, err)

	flags := spanAttrValues(rec, "AppendEvent", "horopter.fold.out_of_order")
	assert.Equal(t, []string{"false", "true"}, flags,
		"the second append carries a lower id than the change's watermark")
}

func TestAppendRecordsTheErrorKindOnAFailure(t *testing.T) {
	rec, restore := recordSpans(t)
	defer restore()

	ctx, s, tn := context.Background(), newStore(t), tenant(t)
	at := time.Date(2026, 9, 15, 10, 0, 0, 0, time.UTC)

	// Subject is required, so Validate rejects this and wraps core.ErrInvalidEvent.
	bad := mkEvent(tn, eventID(t, "bad"), "k1", "dep-3", "deployment.started", at)
	bad.Subject = ""
	_, _, err := s.AppendEvent(ctx, bad)
	require.Error(t, err)

	kinds := spanAttrValues(rec, "AppendEvent", "horopter.error.kind")
	assert.Equal(t, []string{"invalid_event"}, kinds,
		"a validation failure must be recorded as the caller's fault, not a store outage")

	assert.Equal(t, []string{tn}, spanAttrValues(rec, "AppendEvent", "horopter.tenant"),
		"a rejected event must still say whose tenant it came from")
	assert.Equal(t, []string{"deployment.started"},
		spanAttrValues(rec, "AppendEvent", "horopter.event.type"),
		"a rejected event must still say what type it claimed to be")
}
