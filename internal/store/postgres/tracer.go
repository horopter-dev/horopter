package postgres

import (
	"context"
	"time"

	"github.com/exaring/otelpgx"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// ConnectionWaitObserver is told each acquisition's wait, and its error if it failed.
type ConnectionWaitObserver func(wait time.Duration, err error)

type connectionWaitObserverKey struct{}

type acquireStartedKey struct{}

// WithConnectionWaitObserver reports every pool acquisition's wait under ctx to observe.
func WithConnectionWaitObserver(
	ctx context.Context, observe ConnectionWaitObserver,
) context.Context {
	return context.WithValue(ctx, connectionWaitObserverKey{}, observe)
}

// Times acquisitions whether or not spans are recorded, so the signal works without telemetry.
type poolTracer struct {
	*otelpgx.Tracer
}

var (
	_ pgx.QueryTracer       = poolTracer{}
	_ pgx.BatchTracer       = poolTracer{}
	_ pgx.CopyFromTracer    = poolTracer{}
	_ pgx.PrepareTracer     = poolTracer{}
	_ pgx.ConnectTracer     = poolTracer{}
	_ pgxpool.AcquireTracer = poolTracer{}
)

// TraceAcquireStart records when an observed acquisition began.
func (t poolTracer) TraceAcquireStart(
	ctx context.Context, pool *pgxpool.Pool, data pgxpool.TraceAcquireStartData,
) context.Context {
	ctx = t.Tracer.TraceAcquireStart(ctx, pool, data)
	if _, observed := ctx.Value(connectionWaitObserverKey{}).(ConnectionWaitObserver); !observed {
		return ctx
	}
	return context.WithValue(ctx, acquireStartedKey{}, time.Now())
}

// TraceAcquireEnd reports an observed acquisition's wait.
func (t poolTracer) TraceAcquireEnd(
	ctx context.Context, pool *pgxpool.Pool, data pgxpool.TraceAcquireEndData,
) {
	t.Tracer.TraceAcquireEnd(ctx, pool, data)
	observe, observed := ctx.Value(connectionWaitObserverKey{}).(ConnectionWaitObserver)
	started, timed := ctx.Value(acquireStartedKey{}).(time.Time)
	if observed && timed {
		observe(time.Since(started), data.Err)
	}
}
