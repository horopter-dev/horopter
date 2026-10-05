package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// fakeIngress records every posted body and answers with status (201 when 0).
type fakeIngress struct {
	mu     sync.Mutex
	bodies []map[string]any
	status int
	seen   map[string]bool
}

func (f *fakeIngress) server(t *testing.T) string {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		key, _ := body["idempotency_key"].(string)
		f.mu.Lock()
		f.bodies = append(f.bodies, body)
		status := f.status
		replay := f.seen[key]
		if f.seen == nil {
			f.seen = map[string]bool{}
		}
		f.seen[key] = true
		f.mu.Unlock()
		if status != 0 {
			w.WriteHeader(status)
			_, _ = w.Write([]byte(`{"error":"refused by the fake"}`))
			return
		}
		ref, _ := body["correlation_ref"].(string)
		if ref == "" {
			ref = "01J9"
		}
		if replay {
			w.WriteHeader(http.StatusOK)
		} else {
			w.WriteHeader(http.StatusCreated)
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"id": "01J9", "ref": "ci/" + ref, "correlation_ref": ref, "created": !replay})
	}))
	t.Cleanup(srv.Close)
	return srv.URL
}

func (f *fakeIngress) posted() []map[string]any {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]map[string]any(nil), f.bodies...)
}

func record(t *testing.T, url string, args ...string) (int, string, string) {
	t.Helper()
	env := map[string]string{"HOROPTER_INGRESS_URL": url, "HOROPTER_INGRESS_KEY": "pk"}
	var out, errOut bytes.Buffer
	code := run(args, func(k string) string { return env[k] }, &out, &errOut, false)
	return code, out.String(), errOut.String()
}

func TestEventPostsAPointAndPrintsTheAnswer(t *testing.T) {
	f := &fakeIngress{}
	code, out, _ := record(t, f.server(t), "event",
		"--type", "incident.resolved", "--subject", "acme/prod", "--title", "Queue drained",
		"--attr", "team=sre")
	require.Equal(t, 0, code)
	body := f.posted()[0]
	assert.Equal(t, "incident.resolved", body["type"])
	assert.NotContains(t, body, "correlation_ref", "no --ref: a point")
	assert.NotContains(t, body, "source")
	assert.Equal(t, map[string]any{"team": "sre"}, body["attributes"])
	assert.Regexp(t, `^horo/[0-9A-Z]{26}$`, body["idempotency_key"])
	var got map[string]any
	require.NoError(t, json.Unmarshal([]byte(out), &got))
	assert.Equal(t, true, got["created"])
}

func TestAReplayedEventSucceedsWithCreatedFalse(t *testing.T) {
	f := &fakeIngress{}
	url := f.server(t)
	args := []string{"event", "--type", "x.started", "--subject", "s", "--idempotency-key", "k1"}
	code, _, _ := record(t, url, args...)
	require.Equal(t, 0, code)
	code, out, _ := record(t, url, args...)
	require.Equal(t, 0, code)
	var got map[string]any
	require.NoError(t, json.Unmarshal([]byte(out), &got))
	assert.Equal(t, false, got["created"])
	assert.Equal(t, "k1", f.posted()[1]["idempotency_key"])
}

func TestEventWithARefJoinsThatChange(t *testing.T) {
	f := &fakeIngress{}
	code, _, _ := record(t, f.server(t), "event", "--type", "deploy.completed",
		"--subject", "acme/prod", "--ref", "dep-1", "--lifecycle", "completed")
	require.Equal(t, 0, code)
	assert.Equal(t, "dep-1", f.posted()[0]["correlation_ref"])
	assert.Equal(t, "completed", f.posted()[0]["lifecycle"])
}

func TestEventNeedsTypeAndSubject(t *testing.T) {
	code, _, errOut := record(t, (&fakeIngress{}).server(t), "event", "--type", "x.started")
	assert.Equal(t, 2, code)
	assert.Contains(t, errOut, "--subject")
}

func TestEventNamesTheProducerKeyOnA401(t *testing.T) {
	f := &fakeIngress{status: http.StatusUnauthorized}
	code, _, errOut := record(t, f.server(t), "event", "--type", "x.started", "--subject", "s")
	assert.Equal(t, 1, code)
	assert.Contains(t, errOut, "producer key")
	assert.NotContains(t, errOut, "reader key")
}

func TestEventReportsIngresssRefusalOfABadLifecycle(t *testing.T) {
	f := &fakeIngress{status: http.StatusBadRequest}
	code, _, errOut := record(t, f.server(t), "event", "--type", "x.started", "--subject", "s",
		"--lifecycle", "sideways")
	assert.Equal(t, 1, code)
	assert.Contains(t, errOut, "refused by the fake")
}

func TestEventUsageNeverPrintsTheProducerKey(t *testing.T) {
	code, _, errOut := record(t, "http://x", "event", "--tpye", "x")
	assert.Equal(t, 2, code)
	assert.NotContains(t, errOut, "pk")
}

func TestEventWithoutAURLNamesTheIngressVariable(t *testing.T) {
	var out, errOut bytes.Buffer
	code := run([]string{"event", "--type", "x", "--subject", "s"},
		func(string) string { return "" }, &out, &errOut, false)
	assert.Equal(t, 2, code)
	assert.Contains(t, errOut.String(), "HOROPTER_INGRESS_URL")
}
