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

// postBudget bounds one post, including internal/client's own retries.
const (
	postBudget = 30 * time.Second
	retryMax   = 5 * time.Minute
)

// retryBase is a variable so a test can shorten it.
var retryBase = time.Second

// poster queues only a ref's head, so a retried event never lands after a later one.
type poster struct {
	client  *client.Client
	queue   workqueue.TypedRateLimitingInterface[string]
	mu      sync.Mutex
	pending map[string]queued
	byRef   map[string][]string
}

// queued is an event waiting to be posted and the ref it waits in line on.
type queued struct {
	ev  client.NewEvent
	ref string
}

func newPoster(c *client.Client) *poster {
	return &poster{
		client: c,
		queue: workqueue.NewTypedRateLimitingQueue(
			workqueue.NewTypedItemExponentialFailureRateLimiter[string](retryBase, retryMax)),
		pending: map[string]queued{},
		byRef:   map[string][]string{},
	}
}

func orderingRef(ev client.NewEvent) string {
	if ev.CorrelationRef == "" {
		return ev.IdempotencyKey
	}
	return ev.CorrelationRef
}

func (p *poster) enqueue(events []client.NewEvent) {
	p.mu.Lock()
	defer p.mu.Unlock()
	for _, ev := range events {
		key := ev.IdempotencyKey
		if q, ok := p.pending[key]; ok {
			p.pending[key] = queued{ev: ev, ref: q.ref}
			continue
		}
		ref := orderingRef(ev)
		p.pending[key] = queued{ev: ev, ref: ref}
		p.byRef[ref] = append(p.byRef[ref], key)
		if len(p.byRef[ref]) == 1 {
			p.queue.Add(key)
		}
	}
}

// finish drops key, the head of its ref, and queues the ref's next event.
func (p *poster) finish(key string) {
	p.queue.Forget(key)
	p.mu.Lock()
	defer p.mu.Unlock()
	ref := p.pending[key].ref
	delete(p.pending, key)
	rest := p.byRef[ref][1:]
	if len(rest) == 0 {
		delete(p.byRef, ref)
		return
	}
	p.byRef[ref] = rest
	p.queue.Add(rest[0])
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

// refusedForGood reports an answer a later attempt cannot change: a 4xx other than 429.
func refusedForGood(status int) bool {
	return status >= 400 && status < 500 && status != http.StatusTooManyRequests
}

func (p *poster) post(ctx context.Context, key string) {
	p.mu.Lock()
	ev := p.pending[key].ev
	p.mu.Unlock()
	got, err := p.client.Post(ctx, ev, time.Now().Add(postBudget))
	var refused *client.HTTPError
	switch {
	case err == nil:
		slog.Info("recorded", "type", ev.Type, "ref", ev.CorrelationRef, "key", key,
			"created", got.Created)
	case errors.As(err, &refused) && refusedForGood(refused.Status):
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
	p.finish(key)
}
