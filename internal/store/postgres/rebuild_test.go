package postgres_test

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/yo61/horopter/internal/core"
	"github.com/yo61/horopter/internal/store/postgres"
)

func TestRebuildReproducesProjectionExactly(t *testing.T) {
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
	// Event ids come from eventID: event.id is a GLOBAL primary key, so literals
	// collide across tenants and across runs. eventID is backed by a monotonic
	// counter, so ULID-order replay still has a meaningful order to break.
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
	assert.Equal(t, before, after, "a rebuild must reproduce the projection exactly")
}

func TestRebuildIsTenantScoped(t *testing.T) {
	ctx, s, tn := context.Background(), newStore(t), tenant(t)
	at := time.Date(2026, 9, 14, 12, 0, 0, 0, time.UTC)
	other := tn + "-keep"

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
}
