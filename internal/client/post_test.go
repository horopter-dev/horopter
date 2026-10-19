package client_test

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
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

// A Horopter that never answers must not hold the caller for the HTTP client's timeout.
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

// Retrying a 500 for the whole window only delays an error the caller already has.
func TestPostDoesNotRetryA500(t *testing.T) {
	srv, bodies := ingress(t, http.StatusInternalServerError)
	c, err := client.New(srv.URL, "k")
	require.NoError(t, err)
	_, err = c.Post(context.Background(), event(), time.Now().Add(5*time.Second))
	var he *client.HTTPError
	require.ErrorAs(t, err, &he)
	assert.Equal(t, http.StatusInternalServerError, he.Status)
	assert.Len(t, *bodies, 1)
}

// An ingress restart refuses connections for a moment: the case retries exist for.
func TestPostRetriesARefusedConnection(t *testing.T) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	addr := l.Addr().String()
	require.NoError(t, l.Close())
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{"id":"01J9","ref":"ci/r","correlation_ref":"r","created":true}`))
	}))
	t.Cleanup(srv.Close)
	go func() {
		time.Sleep(300 * time.Millisecond)
		l2, err := net.Listen("tcp", addr)
		if err != nil {
			return
		}
		srv.Listener = l2
		srv.Start()
	}()
	c, err := client.New("http://"+addr, "k")
	require.NoError(t, err)
	got, err := c.Post(context.Background(), event(), time.Now().Add(5*time.Second))
	require.NoError(t, err, "a refused connection must be retried within the window")
	assert.True(t, got.Created)
}

func TestPostRetriesAfterAHungAttempt(t *testing.T) {
	client.SetAttemptTimeout(t, 200*time.Millisecond)
	var calls atomic.Int32
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if calls.Add(1) == 1 {
			<-release
			return
		}
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{"id":"01J9","ref":"ci/r","correlation_ref":"r","created":true}`))
	}))
	t.Cleanup(func() { close(release); srv.Close() })
	c, err := client.New(srv.URL, "k")
	require.NoError(t, err)
	_, err = c.Post(context.Background(), event(), time.Now().Add(3*time.Second))
	require.NoError(t, err, "the second attempt must answer")
	assert.EqualValues(t, 2, calls.Load())
}

// A stored post is replayed rather than refused; any other 4xx comes from a hop in front.
func TestRefusedIsAnAnswerThatProvesNothingWasStored(t *testing.T) {
	for status, refused := range map[int]bool{
		http.StatusBadRequest: true, http.StatusUnauthorized: true, http.StatusForbidden: true,
		http.StatusRequestEntityTooLarge: true,
		http.StatusRequestTimeout:        false, http.StatusTooManyRequests: false, 499: false,
		http.StatusUnprocessableEntity: false, http.StatusInternalServerError: false,
		http.StatusServiceUnavailable: false,
	} {
		assert.Equal(t, refused, client.Refused(&client.HTTPError{Status: status}), status)
	}
	assert.False(t, client.Refused(context.DeadlineExceeded), "no answer at all")
	assert.False(t, client.Refused(nil))
}
