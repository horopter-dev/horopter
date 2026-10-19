package postgres_test

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/otel"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	"horopter.dev/horopter/internal/core"
	"horopter.dev/horopter/internal/store/postgres"
)

// Ids follow call order via monotonicEntropy, so arrival and id order coincide here.
func TestRebuildReproducesProjectionForIDOrderedEvents(t *testing.T) {
	ctx, s, tn := context.Background(), newStore(t), tenant(t)
	at := time.Date(2026, 9, 14, 12, 0, 0, 0, time.UTC)
	win := changeQuery(t, tn, at.Add(-24*time.Hour), at.Add(24*time.Hour), 0, nil)

	events := []struct {
		suffix, idem, ref, typ string
		off                    time.Duration
	}{
		{"d1-started", "k1", "d1", "deployment.started", 0},
		{"d1-completed", "k2", "d1", "deployment.completed", 20 * time.Minute},
		{"config", "k3", "", "config.changed", 5 * time.Minute},
		{"d2-started", "k4", "d2", "deployment.started", 30 * time.Minute},
	}
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

	// Unique to this run: a shared literal ref would collide with historical rows.
	sharedRef := eventID(t, "shared-ref")

	// Shared ref, earlier id: a leaked cross-tenant event would fold into other's change.
	_, _, err := s.AppendEvent(ctx, mkEvent(tn, eventID(t, "leak"), "k0", sharedRef,
		"deployment.annotated", at.Add(-2*time.Hour)))
	require.NoError(t, err)

	_, _, err = s.AppendEvent(ctx, mkEvent(other, eventID(t, "other-annotated"), "k1", sharedRef,
		"deployment.annotated", at))
	require.NoError(t, err)

	require.NoError(t, s.RebuildProjection(ctx, tn))

	got, _, err := s.QueryChanges(ctx,
		changeQuery(t, other, at.Add(-time.Hour), at.Add(time.Hour), 0, nil))
	require.NoError(t, err)
	require.Len(t, got, 1, "rebuilding one tenant must not touch another")
	assert.True(t, got[0].Start.Equal(at),
		"other's change must keep its own Start, not tn's earlier same-ref event's — "+
			"a leaked cross-tenant read would merge them under one accumulator key")
}

// The rebuild takes the tenant lock exclusively; appends take it shared.
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
		`SELECT attributes FROM change WHERE tenant = $1 AND ref = $2`, tn, "ci/dep-7").Scan(&attrs)
	require.NoError(t, err)
	assert.Contains(t, attrs, "attr.seeded",
		"the rebuilt row's contents must survive the concurrent append; losing them means "+
			"the append folded from nil over the rebuild")
	assert.Contains(t, attrs, "attr.concurrent", "the concurrent append must survive too")
}

// Not ErrInvalidEvent: the producer retries a 503, so the event is delayed, not lost.
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
		// Margin for the appender to reach lockProjectionWrite; derived from the constant.
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

// Two events on one ref, so events and refs differ.
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

// Fails before the lock; asserts the attributes are emitted at all.
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

func TestRebuildReproducesSubjectFirstSeen(t *testing.T) {
	ctx, s, tn := context.Background(), newStore(t), tenant(t)
	at := time.Date(2026, 10, 6, 14, 0, 0, 0, time.UTC)
	ref := "rebuild-subjects-" + uniqueToken()
	for _, e := range []struct {
		subject, typ string
		d            time.Duration
		lc           core.Lifecycle
	}{
		{"GitRepository/flux-system/flux-system", "flux.artifact.new", 0, core.LifecycleStarted},
		{"Deployment/a/x", "flux.kustomization.applied", 5 * time.Second, core.LifecycleNone},
		{"Kustomization/flux-system/apps", "flux.kustomization.succeeded", 9 * time.Second,
			core.LifecycleCompleted},
		{"Deployment/a/x", "flux.kustomization.applied", 2 * time.Second, core.LifecycleNone},
		{"Service/a/y", "flux.kustomization.applied", time.Hour, core.LifecycleNone},
		{"Kustomization/flux-system/apps", "flux.kustomization.failed", 3 * time.Second,
			core.LifecycleFailed},
	} {
		ev := mkEvent(tn, eventID(t, e.subject), "k-"+uniqueToken(), ref, e.typ, at.Add(e.d))
		ev.Subject, ev.Lifecycle = e.subject, e.lc
		_, _, err := s.AppendEvent(ctx, ev)
		require.NoError(t, err)
	}
	before := projectionRowsForTenant(t, s.Pool(), tn)
	require.Len(t, before, 1)

	require.NoError(t, s.RebuildProjection(ctx, tn))
	assert.Equal(t, before, projectionRowsForTenant(t, s.Pool(), tn))
}

func TestRebuildReproducesTitlesByStart(t *testing.T) {
	ctx, s, tn := context.Background(), newStore(t), tenant(t)
	at := time.Date(2026, 10, 6, 15, 0, 0, 0, time.UTC)
	ref := "titles-" + uniqueToken()
	for _, e := range []struct {
		typ, title string
		d          time.Duration
	}{
		{"deploy.completed", "Done", 2 * time.Minute},
		{"deploy.started", "Deploy v2.5.0", 0},
		{"deploy.progressed", "Halfway", time.Minute},
		{"deploy.started", "Deploy v2.5.0 (retry)", 30 * time.Second},
	} {
		ev := mkEvent(tn, eventID(t, e.title), "k-"+uniqueToken(), ref, e.typ, at.Add(e.d))
		ev.Title = e.title
		_, _, err := s.AppendEvent(ctx, ev)
		require.NoError(t, err)
	}
	before := projectionRowsForTenant(t, s.Pool(), tn)
	require.Len(t, before, 1)
	for _, row := range before {
		assert.Contains(t, row, `"title": "Deploy v2.5.0"`)
	}

	require.NoError(t, s.RebuildProjection(ctx, tn))
	assert.Equal(t, before, projectionRowsForTenant(t, s.Pool(), tn))
}
