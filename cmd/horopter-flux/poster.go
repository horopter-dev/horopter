package main

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"sync"
	"time"

	"k8s.io/client-go/util/workqueue"

	"horopter.dev/horopter/internal/client"
)

// postBudget bounds one post with internal/client's own retries of network errors and 503s.
// Past it the event is requeued with backoff from retryBase to retryMax. Variables so a test
// can shorten them.
var (
	postBudget = 30 * time.Second
	retryBase  = time.Second
	retryMax   = 5 * time.Minute
)

// poster posts events in the order they were queued, one at a time, retrying an event Horopter
// could not take with backoff. Events are queued by idempotency key, so one derived twice
// before it is posted is posted once.
type poster struct {
	client  *client.Client
	queue   workqueue.TypedRateLimitingInterface[string]
	mu      sync.Mutex
	pending map[string]client.NewEvent
}

func newPoster(c *client.Client) *poster {
	return &poster{
		client: c,
		queue: workqueue.NewTypedRateLimitingQueue(
			workqueue.NewTypedItemExponentialFailureRateLimiter[string](retryBase, retryMax)),
		pending: map[string]client.NewEvent{},
	}
}

func (p *poster) enqueue(events []client.NewEvent) {
	p.mu.Lock()
	defer p.mu.Unlock()
	for _, ev := range events {
		p.pending[ev.IdempotencyKey] = ev
		p.queue.Add(ev.IdempotencyKey)
	}
}

// run posts until ctx ends.
func (p *poster) run(ctx context.Context) {
	go func() {
		<-ctx.Done()
		p.queue.ShutDown()
	}()
	for {
		key, shutdown := p.queue.Get()
		if shutdown {
			return
		}
		p.post(ctx, key)
		p.queue.Done(key)
	}
}

func (p *poster) post(ctx context.Context, key string) {
	p.mu.Lock()
	ev := p.pending[key]
	p.mu.Unlock()
	got, err := p.client.Post(ctx, ev, time.Now().Add(postBudget))
	var refused *client.HTTPError
	switch {
	case err == nil:
		slog.Info("recorded", "type", ev.Type, "ref", ev.CorrelationRef, "key", key,
			"created", got.Created)
	case errors.As(err, &refused) && refused.Status != http.StatusServiceUnavailable:
		slog.Error("Horopter refused an event; it is not retried", "type", ev.Type,
			"ref", ev.CorrelationRef, "key", key, "status", refused.Status, "error", err)
	case ctx.Err() != nil:
		return
	default:
		slog.Warn("could not record an event; retrying", "type", ev.Type,
			"ref", ev.CorrelationRef, "key", key, "error", err)
		p.queue.AddRateLimited(key)
		return
	}
	p.queue.Forget(key)
	p.mu.Lock()
	delete(p.pending, key)
	p.mu.Unlock()
}
