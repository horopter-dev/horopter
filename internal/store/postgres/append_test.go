package postgres_test

import (
	"context"
	"crypto/rand"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/oklog/ulid/v2"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	"horopter.dev/horopter/internal/core"
	"horopter.dev/horopter/internal/store/postgres"
	"horopter.dev/horopter/internal/testdb"
)

func newStore(t *testing.T) *postgres.Store {
	t.Helper()
	ctx := context.Background()
	s, err := postgres.Open(ctx, dsn(t))
	require.NoError(t, err)
	t.Cleanup(s.Close)
	// Tests must run sequentially: the concurrency tests install package-level hooks.
	return s
}

// The shared database is never truncated, so ids are unique per process and per call.
var (
	procToken = strconv.FormatInt(time.Now().UnixNano(), 36)
	idSeq     atomic.Uint64
)

func uniqueToken() string {
	return procToken + "-" + strconv.FormatUint(idSeq.Add(1), 36)
}

// Creates the tenant row migration 0005's foreign keys require.
func tenant(t *testing.T) string {
	t.Helper()
	tn := "test-" + t.Name() + "-" + uniqueToken()
	return testdb.NewTenant(t, tn)
}

// Monotonic, so ids sort in call order; not safe for concurrent use.
var monotonicEntropy = ulid.Monotonic(rand.Reader, 0)

// event.id is global across tenants and runs; suffix is logged, not embedded.
func eventID(t *testing.T, suffix string) string {
	t.Helper()
	id := core.NewULID(time.Now(), monotonicEntropy)
	t.Logf("eventID %q -> %s", suffix, id)
	return id
}

// Composes the locus as ingress's resolveRef would; an empty ref is a point event.
func mkEvent(tn, id, idem, producerRef, typ string, at time.Time) core.Event {
	locus := core.LocusFromRow("ci")
	var corr core.Ref
	if producerRef != "" {
		// NewRef's two refusals are unreachable in this branch.
		corr, _ = core.NewRef(locus, producerRef)
	}
	return core.Event{
		ID: id, Tenant: tn, IdempotencyKey: idem, SchemaVersion: core.CurrentSchemaVersion,
		Source: "ci", Type: typ, Subject: "acme/prod", Actor: "ci-bot",
		OccurredAt: at, ReceivedAt: at, CorrelationRef: corr,
		Attributes: map[string]any{"service.version": "2.5.0"}, Origin: core.OriginProcess,
		Locus: locus,
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
		tn, "ci/dep-7").Scan(&ref, &status, &endAt)
	require.NoError(t, err)
	assert.Equal(t, "ci/dep-7", ref)
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
	assert.Equal(t, core.OriginProcess, second.Origin,
		"the duplicate lookup must return the stored origin, not the zero value")

	evs, _, err := s.ListEvents(ctx, eventQuery(t, tn, 0))
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

// Postgres itself must refuse a tenant with no row.
func TestEventTenantForeignKeyIsEnforced(t *testing.T) {
	s := newStore(t)
	missing := "tnt-missing-" + uniqueToken()

	_, err := s.Pool().Exec(context.Background(), `
		INSERT INTO event (id, tenant, schema_version, source, type, subject, actor,
		                   title, occurred_at, received_at, correlation_ref, attributes,
		                   origin, edited, locus)
		VALUES ($1, $2, $3, 'ci', 'deployment.started', 'acme/prod', '', '', now(), now(),
		        'ci/r', '{}'::jsonb, 'process', false, 'ci')`,
		eventID(t, "fk"), missing, core.CurrentSchemaVersion)
	require.Error(t, err, "an event naming a tenant with no row must be refused")
	assert.Contains(t, err.Error(), "event_tenant_fk")
}

func TestChangeTenantForeignKeyIsEnforced(t *testing.T) {
	s := newStore(t)
	missing := "tnt-missing-" + uniqueToken()

	_, err := s.Pool().Exec(context.Background(), `
		INSERT INTO change (tenant, ref, subject, type, title, actor, started_at,
		                    first_seen_at, end_at, status, attributes, last_event_id)
		VALUES ($1, 'ci/r', 'acme/prod', 'deployment.started', '', '', now(), now(), NULL,
		        'in_progress', '{}'::jsonb, 'e0')`,
		missing)
	require.Error(t, err, "a change naming a tenant with no row must be refused")
	assert.Contains(t, err.Error(), "change_tenant_fk")
}

// DEFAULT fires only when the column is omitted, not for an explicit NULL.
func TestAppendEventNormalizesNilAttributes(t *testing.T) {
	ctx, s, tn := context.Background(), newStore(t), tenant(t)
	at := time.Date(2026, 9, 14, 10, 0, 0, 0, time.UTC)
	ev := mkEvent(tn, eventID(t, "1"), "k1", "dep-7", "deployment.started", at)
	ev.Attributes = nil

	_, created, err := s.AppendEvent(ctx, ev)
	require.NoError(t, err, "a nil Attributes map must not violate the NOT NULL column")
	assert.True(t, created)

	evs, _, err := s.ListEvents(ctx, eventQuery(t, tn, 0))
	require.NoError(t, err)
	require.Len(t, evs, 1)
	assert.NotNil(t, evs[0].Attributes,
		"the stored event must round-trip as an empty map, not nil")
	assert.Empty(t, evs[0].Attributes)
}

// At most four, pgxpool's MaxConns floor, or participants queue instead of overlapping.
const concurrentAppends = 4

// Headroom, not a tuned value.
const projectionWriteDelay = 200 * time.Millisecond

// Unseeded: the row-absent path is the one that broke. The hook sleeps; a rendezvous deadlocks.
func TestConcurrentAppendsToSameRefDoNotLoseUpdates(t *testing.T) {
	ctx, s, tn := context.Background(), newStore(t), tenant(t)
	at := time.Date(2026, 9, 14, 10, 0, 0, 0, time.UTC)

	postgres.SetAfterLoadHook(func() { time.Sleep(projectionWriteDelay) })
	t.Cleanup(func() { postgres.SetAfterLoadHook(nil) })

	// Every event is built before any goroutine is released.
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
		`SELECT attributes FROM change WHERE tenant = $1 AND ref = $2`, tn, "ci/dep-7").Scan(&attrs)
	require.NoError(t, err)

	for i := 0; i < concurrentAppends; i++ {
		assert.Contains(t, attrs, fmt.Sprintf("attr%d", i),
			"every concurrent event's contribution must survive; a missing key is a lost update")
	}
}

// Exactly two hook calls proves both goroutines passed the check before either inserted.
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

	evs, _, err := s.ListEvents(ctx, eventQuery(t, tn, 0))
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

// Best-effort restore: OTel's global provider delegates one way, so filter by span name.
func recordSpans(t *testing.T) (*tracetest.SpanRecorder, func()) {
	t.Helper()
	rec := tracetest.NewSpanRecorder()
	prev := otel.GetTracerProvider()
	otel.SetTracerProvider(sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(rec)))
	return rec, func() { otel.SetTracerProvider(prev) }
}

// Spans lacking key are skipped; pass "" to match every span.
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

// Call before newStore: a store takes its counter from the global provider when it opens.
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

// Written by hand: an added key, say a tenant, fails the match.
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

// pgxpool.Close is idempotent, so newStore's own cleanup close stays safe.
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

	// A high id first, then a lower one; literal ids would violate event_pkey on a rerun.
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

// Needs two terminals: a non-terminal event never moves a closed change's status.
func TestAppendRecordsTheReFoldedStatusNotTheIncrementalOne(t *testing.T) {
	rec, restore := recordSpans(t)
	defer restore()

	ctx, s, tn := context.Background(), newStore(t), tenant(t)
	at := time.Date(2026, 9, 15, 10, 0, 0, 0, time.UTC)

	completed := mkEvent(tn, eventID(t, "completed"), "k1", "dep-5", "deployment.completed",
		at.Add(20*time.Minute))
	failed := mkEvent(tn, eventID(t, "failed"), "k2", "dep-5", "deployment.failed", at)
	require.Less(t, completed.ID, failed.ID, "fixture: ids must sort in build order")

	_, _, err := s.AppendEvent(ctx, failed)
	require.NoError(t, err)
	_, _, err = s.AppendEvent(ctx, completed)
	require.NoError(t, err)

	statuses := spanAttrValues(rec, "AppendEvent", "horopter.fold.status")
	assert.Equal(t, []string{"failed", "failed"}, statuses,
		"the second append's incremental fold would have said completed; the span must "+
			"carry the re-folded answer, failed")
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

func newCappedStore(t *testing.T, maxAttributeBytes int) *postgres.Store {
	t.Helper()
	s, err := postgres.Open(context.Background(), dsn(t),
		postgres.WithMaxEventAttributeBytes(maxAttributeBytes))
	require.NoError(t, err)
	t.Cleanup(s.Close)
	return s
}

// 1+17+1+7+1 bytes of JSON, counted by hand.
const mkEventAttributeBytes = 27

func TestAppendRefusesAnEventOverTheAttributeCap(t *testing.T) {
	reader := recordMetrics(t)
	rec, restore := recordSpans(t)
	defer restore()
	ctx, tn := context.Background(), tenant(t)
	s := newCappedStore(t, mkEventAttributeBytes-1)
	at := time.Date(2026, 9, 17, 10, 0, 0, 0, time.UTC)

	_, _, err := s.AppendEvent(ctx,
		mkEvent(tn, eventID(t, "big"), "k1", "dep-cap", "deployment.started", at))

	require.ErrorIs(t, err, core.ErrInvalidEvent)
	assert.Contains(t, err.Error(), "attributes encode to 27 bytes of JSON; the limit is 26")
	evs, _, err := s.ListEvents(ctx, eventQuery(t, tn, 0))
	require.NoError(t, err)
	assert.Empty(t, evs, "a refused event must not be stored")
	assert.Equal(t, map[string]int64{appendFailedInvalid: 1}, appendCounts(t, reader))
	assert.Equal(t, []string{"invalid_event"},
		spanAttrValues(rec, "AppendEvent", "horopter.error.kind"))
}

func TestAppendAcceptsAnEventAtTheAttributeCap(t *testing.T) {
	ctx, tn := context.Background(), tenant(t)
	s := newCappedStore(t, mkEventAttributeBytes)
	at := time.Date(2026, 9, 17, 10, 0, 0, 0, time.UTC)

	_, created, err := s.AppendEvent(ctx,
		mkEvent(tn, eventID(t, "fits"), "k1", "dep-fit", "deployment.started", at))

	require.NoError(t, err)
	assert.True(t, created)
}

func TestAppendAppliesNoAttributeCapByDefault(t *testing.T) {
	ctx, s, tn := context.Background(), newStore(t), tenant(t)
	ev := mkEvent(tn, eventID(t, "large"), "k1", "dep-large", "deployment.started",
		time.Date(2026, 9, 17, 10, 0, 0, 0, time.UTC))
	ev.Attributes = map[string]any{"horopter.description": strings.Repeat("x", 64<<10)}

	_, created, err := s.AppendEvent(ctx, ev)

	require.NoError(t, err)
	assert.True(t, created, "a store opened without WithMaxEventAttributeBytes caps nothing")
}

// newStore runs first: OpenNoMigrate needs the schema.
func TestOpenNoMigrateAppliesTheAttributeCap(t *testing.T) {
	ctx, tn := context.Background(), tenant(t)
	newStore(t)
	s, err := postgres.OpenNoMigrate(ctx, dsn(t),
		postgres.WithMaxEventAttributeBytes(mkEventAttributeBytes-1))
	require.NoError(t, err)
	t.Cleanup(s.Close)

	_, _, err = s.AppendEvent(ctx, mkEvent(tn, eventID(t, "big"), "k1", "dep-cap",
		"deployment.started", time.Date(2026, 9, 17, 10, 0, 0, 0, time.UTC)))

	require.ErrorIs(t, err, core.ErrInvalidEvent)
}

// uq_event_idem is partial on IS NOT NULL, so an empty key must bind as NULL.
func TestTwoKeylessEventsInOneTenantAreBothStored(t *testing.T) {
	ctx, s, tn := context.Background(), newStore(t), tenant(t)
	at := time.Date(2026, 10, 5, 9, 0, 0, 0, time.UTC)

	for _, suffix := range []string{"first", "second"} {
		ev := mkEvent(tn, eventID(t, suffix), "", "", "config.changed", at)
		_, created, err := s.AppendEvent(ctx, ev)
		require.NoError(t, err)
		assert.True(t, created, "%s keyless event must be stored, not matched as a duplicate", suffix)
	}
	var n int
	require.NoError(t, s.Pool().QueryRow(ctx,
		`SELECT count(*) FROM event WHERE tenant = $1`, tn).Scan(&n))
	assert.Equal(t, 2, n)
}

// Only uq_event_idem means a concurrent retry; any other unique violation surfaces as itself.
func TestAnEventIDCollisionIsNotTakenForAConcurrentDuplicate(t *testing.T) {
	ctx, s, tn := context.Background(), newStore(t), tenant(t)
	at := time.Date(2026, 10, 5, 9, 0, 0, 0, time.UTC)
	id := eventID(t, "shared")

	_, _, err := s.AppendEvent(ctx, mkEvent(tn, id, "k1", "", "config.changed", at))
	require.NoError(t, err)
	_, _, err = s.AppendEvent(ctx, mkEvent(tn, id, "k2", "", "config.changed", at))

	var pgErr *pgconn.PgError
	require.ErrorAs(t, err, &pgErr, "the store must surface Postgres's own violation")
	assert.Equal(t, "event_pkey", pgErr.ConstraintName)
}

// Without the subject map, each append would keep only its own subject.
func TestAppendKeepsEarlierSubjects(t *testing.T) {
	ctx, s, tn := context.Background(), newStore(t), tenant(t)
	at := time.Date(2026, 10, 6, 9, 0, 0, 0, time.UTC)
	ref := "subjects-" + uniqueToken()
	first := mkEvent(tn, eventID(t, "a"), "k-a-"+uniqueToken(), ref, "deployment.started", at)
	first.Subject = "Deployment/a/x"
	second := mkEvent(tn, eventID(t, "b"), "k-b-"+uniqueToken(), ref, "deployment.annotated",
		at.Add(time.Minute))
	second.Subject = "Service/a/y"
	for _, ev := range []core.Event{first, second} {
		_, _, err := s.AppendEvent(ctx, ev)
		require.NoError(t, err)
	}

	var got map[string]int64
	require.NoError(t, s.Pool().QueryRow(ctx,
		`SELECT subject_first_seen FROM change WHERE tenant = $1 AND ref = $2`,
		tn, core.RefFor(first).String()).Scan(&got))
	assert.Equal(t, map[string]int64{
		"Deployment/a/x": at.UnixMicro(),
		"Service/a/y":    at.Add(time.Minute).UnixMicro(),
	}, got)
}
