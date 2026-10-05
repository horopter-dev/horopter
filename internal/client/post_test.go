package client_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"horopter.dev/horopter/internal/client"
)

// ingress answers with the statuses given, in order, then 201; it records each body.
func ingress(t *testing.T, statuses ...int) (*httptest.Server, *[]map[string]any) {
	t.Helper()
	var mu sync.Mutex
	var bodies []map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, "/v1/events", r.URL.Path)
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		mu.Lock()
		bodies = append(bodies, body)
		i := len(bodies) - 1
		mu.Unlock()
		if i < len(statuses) {
			w.WriteHeader(statuses[i])
			_, _ = w.Write([]byte(`{"error":"try later"}`))
			return
		}
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{"id":"01J9","ref":"ci/r","correlation_ref":"r","created":true}`))
	}))
	t.Cleanup(srv.Close)
	return srv, &bodies
}

func event() client.NewEvent {
	return client.NewEvent{Type: "deployment.started", Subject: "acme/prod",
		CorrelationRef: "r", IdempotencyKey: "horo/01J/start", Lifecycle: "started",
		OccurredAt: time.Date(2026, 10, 5, 9, 0, 0, 0, time.UTC)}
}

func TestPostRetriesA503WithTheSameKey(t *testing.T) {
	srv, bodies := ingress(t, http.StatusServiceUnavailable)
	c, err := client.New(srv.URL, "k")
	require.NoError(t, err)
	got, err := c.Post(context.Background(), event(), time.Now().Add(5*time.Second))
	require.NoError(t, err)
	want := client.Appended{ID: "01J9", Ref: "ci/r", CorrelationRef: "r", Created: true}
	assert.Equal(t, want, got)
	require.Len(t, *bodies, 2)
	assert.Equal(t, (*bodies)[0]["idempotency_key"], (*bodies)[1]["idempotency_key"])
}

func TestPostDoesNotRetryA4xx(t *testing.T) {
	srv, bodies := ingress(t, http.StatusBadRequest)
	c, err := client.New(srv.URL, "k")
	require.NoError(t, err)
	_, err = c.Post(context.Background(), event(), time.Now().Add(5*time.Second))
	var he *client.HTTPError
	require.ErrorAs(t, err, &he)
	assert.Equal(t, http.StatusBadRequest, he.Status)
	assert.Len(t, *bodies, 1)
}

func TestPostStopsRetryingAtItsDeadline(t *testing.T) {
	srv, _ := ingress(t, 503, 503, 503, 503, 503, 503, 503, 503, 503, 503, 503, 503)
	c, err := client.New(srv.URL, "k")
	require.NoError(t, err)
	start := time.Now()
	_, err = c.Post(context.Background(), event(), time.Now().Add(400*time.Millisecond))
	require.Error(t, err)
	assert.Less(t, time.Since(start), 2*time.Second)
}

// TestPostIsBoundedAgainstAServerThatNeverAnswers: the deadline bounds every attempt, so a
// Horopter that accepts a connection and never answers cannot hold the caller for the HTTP
// client's own timeout.
func TestPostIsBoundedAgainstAServerThatNeverAnswers(t *testing.T) {
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		<-release
	}))
	t.Cleanup(func() { close(release); srv.Close() })
	c, err := client.New(srv.URL, "k")
	require.NoError(t, err)
	start := time.Now()
	_, err = c.Post(context.Background(), event(), time.Now().Add(500*time.Millisecond))
	require.Error(t, err)
	assert.Less(t, time.Since(start), 2*time.Second)
}

func TestPostNeverSendsSource(t *testing.T) {
	srv, bodies := ingress(t)
	c, err := client.New(srv.URL, "k")
	require.NoError(t, err)
	_, err = c.Post(context.Background(), event(), time.Now().Add(5*time.Second))
	require.NoError(t, err)
	assert.NotContains(t, (*bodies)[0], "source")
	assert.Equal(t, "started", (*bodies)[0]["lifecycle"])
	assert.Equal(t, "2026-10-05T09:00:00Z", (*bodies)[0]["occurred_at"])
}
