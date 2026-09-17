package postgres

import (
	"context"
	"time"

	"github.com/exaring/otelpgx"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// ConnectionWaitObserver is told how long one pool connection acquisition waited, and the
// error if it failed.
type ConnectionWaitObserver func(wait time.Duration, err error)

type connectionWaitObserverKey struct{}

type acquireStartedKey struct{}

// WithConnectionWaitObserver returns a context under which every connection the store acquires
// from its pool reports its wait to observe. Egress uses the waits as its overload signal.
func WithConnectionWaitObserver(
	ctx context.Context, observe ConnectionWaitObserver,
) context.Context {
	return context.WithValue(ctx, connectionWaitObserverKey{}, observe)
}

// poolTracer is the tracer every pool is built with. Embedding otelpgx's tracer delegates
// every tracing interface it implements, so statement and acquire spans are unchanged; the two
// acquire methods are overridden only to time the wait for an observer on the context.
//
// The timing does not depend on a span being recorded, so the overload signal works with
// telemetry disabled.
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
