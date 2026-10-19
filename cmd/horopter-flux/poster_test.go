package main

import (
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"horopter.dev/horopter/internal/client"
)

// startPoster's backoff lets a test tell a retried event from one posted behind it.
func startPoster(t *testing.T, url string, backoff time.Duration) *poster {
	t.Helper()
	retryBase = backoff
	t.Cleanup(func() { retryBase = time.Second })
	c, err := client.New(url, "hpk_test")
	require.NoError(t, err)
	p := newPoster(c)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		p.run(ctx)
		close(done)
	}()
	t.Cleanup(func() {
		cancel()
		<-done
	})
	return p
}

func refEvent(key, ref string) client.NewEvent {
	return client.NewEvent{
		Type: "flux.test", Subject: "HelmRelease/horopter/horopter", CorrelationRef: ref,
		IdempotencyKey: key, Lifecycle: "none",
		OccurredAt: time.Date(2026, 10, 12, 0, 0, 0, 0, time.UTC),
	}
}

// refusedFirst answers status to key's first post and accepts every other.
func refusedFirst(status int, refused string) func(string, int) int {
	return func(key string, attempt int) int {
		if key == refused && attempt == 1 {
			return status
		}
		return http.StatusCreated
	}
}

func TestALaterEventOnARefWaitsWhileAnEarlierOneIsRetried(t *testing.T) {
	in, url := newIngest(t)
	in.respond = refusedFirst(http.StatusInternalServerError, "attempted")
	p := startPoster(t, url, 200*time.Millisecond)

	p.enqueue([]client.NewEvent{refEvent("attempted", "v11"), refEvent("completed", "v11")})

	require.Eventually(t, func() bool { return len(in.recordedKeys()) == 2 },
		5*time.Second, 10*time.Millisecond)
	assert.Equal(t, []string{"attempted", "completed"}, in.recordedKeys())
	assert.Equal(t, 1, in.attempts("completed"))
}

func TestAnotherRefIsPostedWhileOneIsRetried(t *testing.T) {
	in, url := newIngest(t)
	in.respond = func(key string, _ int) int {
		if key == "v11/attempted" {
			return http.StatusInternalServerError
		}
		return http.StatusCreated
	}
	p := startPoster(t, url, 20*time.Millisecond)

	p.enqueue([]client.NewEvent{
		refEvent("v11/attempted", "v11"), refEvent("v11/completed", "v11"),
		refEvent("commit", "sha1:b1f2477"),
	})

	require.Eventually(t, func() bool {
		return in.attempts("commit") == 1 && in.attempts("v11/attempted") >= 3
	}, 5*time.Second, 10*time.Millisecond)
	assert.Zero(t, in.attempts("v11/completed"))
}

func TestAnEventRefusedForGoodReleasesItsRef(t *testing.T) {
	in, url := newIngest(t)
	in.respond = refusedFirst(http.StatusUnprocessableEntity, "attempted")
	p := startPoster(t, url, time.Hour)

	p.enqueue([]client.NewEvent{refEvent("attempted", "v11"), refEvent("completed", "v11")})

	require.Eventually(t, func() bool { return in.attempts("completed") == 1 },
		5*time.Second, 10*time.Millisecond)
	assert.Equal(t, []string{"completed"}, in.recordedKeys())
}

func TestUncorrelatedEventsDoNotWaitForEachOther(t *testing.T) {
	in, url := newIngest(t)
	in.respond = refusedFirst(http.StatusInternalServerError, "drift-1")
	p := startPoster(t, url, time.Hour)

	p.enqueue([]client.NewEvent{refEvent("drift-1", ""), refEvent("drift-2", "")})

	require.Eventually(t, func() bool { return in.attempts("drift-2") == 1 },
		5*time.Second, 10*time.Millisecond)
	assert.Equal(t, []string{"drift-2"}, in.recordedKeys())
}

func TestAnEventDerivedAgainWhileWaitingKeepsItsPlaceAndItsLatestBody(t *testing.T) {
	in, url := newIngest(t)
	release := make(chan struct{})
	in.respond = func(key string, _ int) int {
		if key == "started" {
			in.mu.Unlock()
			<-release
			in.mu.Lock()
		}
		return http.StatusCreated
	}
	p := startPoster(t, url, time.Hour)
	p.enqueue([]client.NewEvent{
		refEvent("started", "v11"), refEvent("attempted", "v11"), refEvent("completed", "v11"),
	})
	require.Eventually(t, func() bool { return in.attempts("started") == 1 },
		5*time.Second, 10*time.Millisecond)

	again := refEvent("attempted", "v11")
	again.Title = "0.3.2"
	p.enqueue([]client.NewEvent{again})
	close(release)

	require.Eventually(t, func() bool { return len(in.recordedKeys()) == 3 },
		5*time.Second, 10*time.Millisecond)
	assert.Equal(t, []string{"started", "attempted", "completed"}, in.recordedKeys())
	assert.Equal(t, "0.3.2", in.body("attempted")["title"])
}

func TestAnEventDerivedAgainWhileItsRetryWaitsIsPostedAsLastDerived(t *testing.T) {
	in, url := newIngest(t)
	in.respond = refusedFirst(http.StatusTooManyRequests, "attempted")
	p := startPoster(t, url, 200*time.Millisecond)
	p.enqueue([]client.NewEvent{refEvent("attempted", "v11")})
	require.Eventually(t, func() bool { return in.attempts("attempted") == 1 },
		5*time.Second, 10*time.Millisecond)

	again := refEvent("attempted", "v11")
	again.Title = "0.3.2"
	p.enqueue([]client.NewEvent{again})

	require.Eventually(t, func() bool { return in.attempts("attempted") == 2 },
		5*time.Second, 10*time.Millisecond)
	in.mu.Lock()
	defer in.mu.Unlock()
	assert.Equal(t, "0.3.2", in.posts["attempted"][1]["title"])
}

func TestAnEventDerivedAgainOnAnotherRefWaitsOnTheRefItWasQueuedOn(t *testing.T) {
	in, url := newIngest(t)
	in.respond = refusedFirst(http.StatusInternalServerError, "started")
	p := startPoster(t, url, 200*time.Millisecond)
	p.enqueue([]client.NewEvent{
		refEvent("started", "v11"), refEvent("attempted", "v11"), refEvent("commit", "sha1"),
	})
	p.enqueue([]client.NewEvent{refEvent("attempted", "sha1")})

	require.Eventually(t, func() bool { return len(in.recordedKeys()) == 3 },
		5*time.Second, 10*time.Millisecond)
	assert.Equal(t, []string{"commit", "started", "attempted"}, in.recordedKeys())
}
