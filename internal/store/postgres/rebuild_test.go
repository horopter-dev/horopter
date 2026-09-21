package postgres_test

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/horopter-dev/horopter/internal/core"
	"github.com/horopter-dev/horopter/internal/store/postgres"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/otel"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
)

// TestRebuildReproducesProjectionForIDOrderedEvents pins RebuildProjection's guarantee
// at the strength it actually has, not the stronger claim its former name made.
// RebuildProjection's own doc comment explains why "exactly" was never quite true: live
// appends fold in ARRIVAL order, while a rebuild replays in ID order, and the two only
// coincide when ids sort the same way the events arrived. This test's ids come from
// eventID, backed by monotonicEntropy precisely so that coincidence is guaranteed here
// rather than left to chance — what this test actually proves is the id-ordered fold
// production relies on when that condition holds, not that a rebuild is exact in
// general.
func TestRebuildReproducesProjectionForIDOrderedEvents(t *testing.T) {
	ctx, s, tn := context.Background(), newStore(t), tenant(t)
	at := time.Date(2026, 9, 14, 12, 0, 0, 0, time.UTC)
	win := postgres.ChangeQuery{
		Tenant: tn, From: at.Add(-24 * time.Hour), To: at.Add(24 * time.Hour),
	}

	events := []struct {
		suffix, idem, ref, typ string
		off                    time.Duration
	}{
		{"d1-started", "k1", "d1", "deployment.started", 0},
		{"d1-completed", "k2", "d1", "deployment.completed", 20 * time.Minute},
		{"config", "k3", "", "config.changed", 5 * time.Minute},
		{"d2-started", "k4", "d2", "deployment.started", 30 * time.Minute},
	}
	// event.id is a GLOBAL primary key, so literals collide across tenants and across
	// runs; eventID's ids are both unique and, thanks to monotonicEntropy, ordered by
	// call order, which is what makes the id-ordered rebuild below comparable to the
	// arrival-ordered live fold above it at all.
	for _, e := range events {
		_, _, err := s.AppendEvent(ctx,
			mkEvent(tn, eventID(t, e.suffix), e.idem, e.ref, e.typ, at.Add(e.off)))
		require.NoError(t, err)
	}

	before, _, err := s.QueryChanges(ctx, win)
	require.NoError(t, err)
	require.Len(t, before, 3)

	require.NoError(t, s.RebuildProjection(ctx, tn))

	after, _, err := s.QueryChanges(ctx, win)
	require.NoError(t, err)
	assert.Equal(t, before, after,
		"a rebuild must reproduce the projection when ids sort in arrival order")
}

func TestRebuildIsTenantScoped(t *testing.T) {
	ctx, s, tn := context.Background(), newStore(t), tenant(t)
	at := time.Date(2026, 9, 14, 12, 0, 0, 0, time.UTC)
	other := siblingTenant(t, tn, "keep")

	// sharedRef must be unique to this test run, not a common literal like "d1": the
	// tests in this package share one never-truncated database, and "d1" is reused by
	// many of them across many past runs, so a leak test built on that literal would be
	// corrupted by unrelated historical rows sharing the same ref — producing a false
	// pass instead of a real guarantee. eventID's counter-backed uniqueness (reused here
	// for a ref, not an id) guarantees no other test, past or future, can collide on it.
	sharedRef := eventID(t, "shared-ref")

	// tn's event shares that ref with other's event below and is given an earlier id,
	// so it sorts first if RebuildProjection's event-log read ever lost its tenant
	// filter. RebuildProjection's accumulator is keyed by ref alone, so a leaked
	// cross-tenant event with a colliding ref would fold into the SAME accumulator
	// entry as other's own event and corrupt it on upsert — even though the row count
	// stays 1. A single-event, non-colliding-ref tenant-scope test can never observe
	// that class of bug. Both events use a non-start, non-terminal type
	// ("deployment.annotated") so Start is set only by Fold's zero-value fallback:
	// whichever event folds first wins, making a leak visible as other's Start
	// silently becoming tn's earlier one.
	_, _, err := s.AppendEvent(ctx, mkEvent(tn, eventID(t, "leak"), "k0", sharedRef,
		"deployment.annotated", at.Add(-2*time.Hour)))
	require.NoError(t, err)

	_, _, err = s.AppendEvent(ctx, mkEvent(other, eventID(t, "other-annotated"), "k1", sharedRef,
		"deployment.annotated", at))
	require.NoError(t, err)

	require.NoError(t, s.RebuildProjection(ctx, tn))

	got, _, err := s.QueryChanges(ctx, postgres.ChangeQuery{
		Tenant: other, From: at.Add(-time.Hour), To: at.Add(time.Hour),
	})
	require.NoError(t, err)
	require.Len(t, got, 1, "rebuilding one tenant must not touch another")
	assert.True(t, got[0].Start.Equal(at),
		"other's change must keep its own Start, not tn's earlier same-ref event's — "+
			"a leaked cross-tenant read would merge them under one accumulator key")
}

// TestRebuildDoesNotLoseConcurrentAppends is the reason RebuildProjection takes the
// tenant's advisory lock exclusively and applyToProjection takes it shared.
//
// The rebuild's DELETE and the appender's read are the dangerous pair. An appender that
// blocks on a row the rebuild has deleted re-checks after the rebuild commits, finds
// nothing — the rebuilt row is not in its snapshot — folds from nil and overwrites the
// rebuilt row, destroying every earlier event's contribution. It is the same lost-update
// mechanism as TestConcurrentAppendsToSameRefDoNotLoseUpdates, reached from the other
// side. A per-ref lock cannot prevent it: a rebuild touches refs no appender holds, so
// there is no per-ref lock for it to contend on.
//
// afterDeleteHook makes the ordering exact rather than probable: it releases the
// appender only once the DELETE has run, so the append provably starts inside the
// window, and then holds the rebuild there long enough for the append to reach its lock.
// Like afterLoadHook it must be a sleep, not a rendezvous — with the fix in place the
// appender cannot get past the tenant lock until the rebuild commits, so a hook that
// waited for it would deadlock in exactly the case that must pass.
func TestRebuildDoesNotLoseConcurrentAppends(t *testing.T) {
	ctx, s, tn := context.Background(), newStore(t), tenant(t)
	at := time.Date(2026, 9, 14, 12, 0, 0, 0, time.UTC)

	seeded := mkEvent(tn, eventID(t, "seed"), "k0", "dep-7", "deployment.started", at)
	seeded.Attributes = map[string]any{"attr.seeded": "set"}
	_, _, err := s.AppendEvent(ctx, seeded)
	require.NoError(t, err)

	released := make(chan struct{})
	var once sync.Once
	postgres.SetAfterDeleteHook(func() {
		once.Do(func() { close(released) })
		time.Sleep(projectionWriteDelay)
	})
	t.Cleanup(func() { postgres.SetAfterDeleteHook(nil) })

	appended := make(chan error, 1)
	go func() {
		<-released
		ev := mkEvent(tn, eventID(t, "concurrent"), "k1", "dep-7", "deployment.annotated",
			at.Add(time.Minute))
		ev.Attributes = map[string]any{"attr.concurrent": "set"}
		_, _, err := s.AppendEvent(ctx, ev)
		appended <- err
	}()

	require.NoError(t, s.RebuildProjection(ctx, tn))
	require.NoError(t, <-appended)

	var attrs map[string]any
	err = s.Pool().QueryRow(ctx,
		`SELECT attributes FROM change WHERE tenant = $1 AND ref = $2`, tn, "dep-7").Scan(&attrs)
	require.NoError(t, err)
	assert.Contains(t, attrs, "attr.seeded",
		"the rebuilt row's contents must survive the concurrent append; losing them means "+
			"the append folded from nil over the rebuild")
	assert.Contains(t, attrs, "attr.concurrent", "the concurrent append must survive too")
}

// TestAppendFailsFastWhileARebuildHoldsTheTenantLock covers the operational hazard the
// lock bound exists for. RebuildProjection holds a tenant's advisory lock exclusively for
// its whole run; without a bound every append for that tenant waits indefinitely, each one
// pinning a connection from a pool shared across tenants, so a long rebuild stalls ingress
// for tenants that have nothing to do with it.
//
// The error must NOT be a core.ErrInvalidEvent, which is what routes it to 503 rather than
// 400 in ingress, and is the entire point: the producer retries, the retry lands after the
// rebuild, and the event is delayed rather than lost.
//
// afterDeleteHook holds the rebuild open past AppendLockTimeout, which is what makes the
// timeout fire rather than the append merely being slow. Like the other hooks it must be a
// sleep, not a rendezvous: the appender cannot reach any rendezvous while the lock is held.
func TestAppendFailsFastWhileARebuildHoldsTheTenantLock(t *testing.T) {
	rec, restore := recordSpans(t)
	defer restore()

	ctx, s, tn := context.Background(), newStore(t), tenant(t)
	at := time.Date(2026, 9, 14, 12, 0, 0, 0, time.UTC)

	_, _, err := s.AppendEvent(ctx,
		mkEvent(tn, eventID(t, "seed"), "k0", "dep-7", "deployment.started", at))
	require.NoError(t, err)

	released := make(chan struct{})
	var once sync.Once
	postgres.SetAfterDeleteHook(func() {
		once.Do(func() { close(released) })
		// The margin must cover the appender reaching lockProjectionWrite — a Begin, a
		// set_config and an event INSERT — within it. Too small a margin and the append
		// acquires the lock after the rebuild releases it instead of timing out, failing
		// with "An error is expected but got nil". Derived from the constant so the two
		// cannot drift apart.
		time.Sleep(postgres.AppendLockTimeout + 2*time.Second)
	})
	t.Cleanup(func() { postgres.SetAfterDeleteHook(nil) })

	appended := make(chan error, 1)
	go func() {
		<-released
		_, _, err := s.AppendEvent(ctx, mkEvent(tn, eventID(t, "blocked"), "k1", "dep-7",
			"deployment.annotated", at.Add(time.Minute)))
		appended <- err
	}()

	require.NoError(t, s.RebuildProjection(ctx, tn))

	err = <-appended
	require.Error(t, err,
		"an append blocked by a rebuild must give up rather than hold a pool connection "+
			"for the rebuild's whole duration")
	assert.False(t, errors.Is(err, core.ErrInvalidEvent),
		"a lock timeout is transient, not the caller's fault; wrapping ErrInvalidEvent "+
			"would make ingress answer 400 and the producer discard the event")
	assert.Contains(t, err.Error(), "rebuild",
		"the message must let an operator tell a running rebuild from a dead database")
	assert.Contains(t, err.Error(), "nothing was written",
		"the message must say the append is safe to retry")

	assert.Contains(t, spanAttrValues(rec, "AppendEvent", "horopter.lock.timed_out"), "true",
		"the blocked append's span must record the timeout, not just the returned error")
}

// TestRebuildRecordsItsShapeOnASpan pins RebuildProjection's span attributes: the
// tenant, the events replayed and the refs they folded into say what the rebuild did,
// and their ratio predicts how long the NEXT rebuild for this tenant will hold the lock.
//
// Two events land on the SAME ref, so events (2) and refs (1) differ. A single seeded
// event, as this test used to do, makes both values 1 — indistinguishable from a
// transposed replayProjection that swapped its two named results.
func TestRebuildRecordsItsShapeOnASpan(t *testing.T) {
	rec := tracetest.NewSpanRecorder()
	prev := otel.GetTracerProvider()
	otel.SetTracerProvider(sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(rec)))
	t.Cleanup(func() { otel.SetTracerProvider(prev) })

	ctx, s, tn := context.Background(), newStore(t), tenant(t)
	at := time.Date(2026, 9, 15, 10, 0, 0, 0, time.UTC)
	_, _, err := s.AppendEvent(ctx, mkEvent(tn, eventID(t, "a"), "k1", "dep-1",
		"deployment.started", at))
	require.NoError(t, err)
	_, _, err = s.AppendEvent(ctx, mkEvent(tn, eventID(t, "b"), "k2", "dep-1",
		"deployment.completed", at.Add(20*time.Minute)))
	require.NoError(t, err)

	require.NoError(t, s.RebuildProjection(ctx, tn))

	var attrs map[string]any
	for _, span := range rec.Ended() {
		if span.Name() == "RebuildProjection" {
			attrs = map[string]any{}
			for _, kv := range span.Attributes() {
				attrs[string(kv.Key)] = kv.Value.AsInterface()
			}
		}
	}
	require.NotNil(t, attrs, "RebuildProjection must open a span")
	assert.Equal(t, tn, attrs["horopter.rebuild.tenant"])
	assert.Equal(t, int64(2), attrs["horopter.rebuild.events"])
	assert.Equal(t, int64(1), attrs["horopter.rebuild.refs"])
}

// TestFailedRebuildStillRecordsItsTenant is the failure half of the rebuild span. The
// case that matters operationally is RebuildLockTimeout, where an operator needs the span
// to say which tenant was being rebuilt and how long it waited; recording the attributes
// after the replay meant a rebuild that never got the lock described nothing.
//
// A cancelled context fails the Begin, which is the cheapest deterministic failure and
// exercises the same deferred block. It fails BEFORE the lock, so lock_wait_ms is
// correctly zero here and is not what this asserts — what it asserts is that the
// attributes are emitted at all on a path that never reaches the replay.
func TestFailedRebuildStillRecordsItsTenant(t *testing.T) {
	rec, restore := recordSpans(t)
	defer restore()

	s, tn := newStore(t), tenant(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	require.Error(t, s.RebuildProjection(ctx, tn))

	assert.Equal(t, []string{tn},
		spanAttrValues(rec, "RebuildProjection", "horopter.rebuild.tenant"),
		"a rebuild that failed before replaying anything must still name its tenant")
	assert.Equal(t, []string{"0"},
		spanAttrValues(rec, "RebuildProjection", "horopter.rebuild.events"))
}
