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

// fakeRefold answers every re-fold with status and body, recording each request's body and
// Authorization header.
type fakeRefold struct {
	mu     sync.Mutex
	bodies []map[string]string
	keys   []string
	status int
	answer string
}

func (f *fakeRefold) server(t *testing.T) string {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]string
		_ = json.NewDecoder(r.Body).Decode(&body)
		f.mu.Lock()
		f.bodies = append(f.bodies, body)
		f.keys = append(f.keys, r.Header.Get("Authorization"))
		f.mu.Unlock()
		if f.status != 0 {
			w.WriteHeader(f.status)
		}
		_, _ = w.Write([]byte(f.answer))
	}))
	t.Cleanup(srv.Close)
	return srv.URL
}

func runRefoldWith(t *testing.T, env map[string]string, args ...string) (int, string, string) {
	t.Helper()
	var out, errOut bytes.Buffer
	code := run(append([]string{"refold"}, args...), func(k string) string { return env[k] },
		&out, &errOut, false)
	return code, out.String(), errOut.String()
}

func adminEnv(url string) map[string]string {
	return map[string]string{"HOROPTER_INGRESS_URL": url, "HOROPTER_ADMIN_KEY": "horo_ADMIN"}
}

func TestRefoldPrintsIngresssAnswer(t *testing.T) {
	f := &fakeRefold{answer: `{"ref":"ci/d1","events":2,"altered":["status"]}`}
	code, out, _ := runRefoldWith(t, adminEnv(f.server(t)), "ci/d1")
	require.Equal(t, 0, code)
	assert.JSONEq(t, `{"ref":"ci/d1","events":2,"altered":["status"]}`, out)
	assert.Equal(t, map[string]string{"ref": "ci/d1"}, f.bodies[0])
	assert.Equal(t, "Bearer horo_ADMIN", f.keys[0])
}

func TestRefoldSendsTheTenantForAGlobalKey(t *testing.T) {
	f := &fakeRefold{answer: `{"ref":"ci/d1","events":1,"altered":[]}`}
	code, _, _ := runRefoldWith(t, adminEnv(f.server(t)), "--tenant", "acme", "ci/d1")
	require.Equal(t, 0, code)
	assert.Equal(t, "acme", f.bodies[0]["tenant"])
}

func TestRefoldOfAnUnknownRefExitsThree(t *testing.T) {
	f := &fakeRefold{status: http.StatusNotFound, answer: `{"error":"no events for ref"}`}
	code, _, errOut := runRefoldWith(t, adminEnv(f.server(t)), "ci/nope")
	assert.Equal(t, 3, code)
	assert.Contains(t, errOut, "no events for ref")
}

func TestARefusedRefoldNamesTheAdminKey(t *testing.T) {
	for _, status := range []int{http.StatusUnauthorized, http.StatusForbidden} {
		f := &fakeRefold{status: status, answer: `{"error":"refused"}`}
		code, _, errOut := runRefoldWith(t, adminEnv(f.server(t)), "ci/d1")
		assert.Equal(t, 1, code)
		assert.Contains(t, errOut, "HOROPTER_ADMIN_KEY")
	}
}

// TestRefoldNeverUsesTheProducerKey: a producer key in HOROPTER_INGRESS_KEY must not be sent
// to an admin route; without HOROPTER_ADMIN_KEY nothing is sent at all.
func TestRefoldNeverUsesTheProducerKey(t *testing.T) {
	f := &fakeRefold{answer: `{}`}
	env := map[string]string{"HOROPTER_INGRESS_URL": f.server(t), "HOROPTER_INGRESS_KEY": "pk"}
	code, _, errOut := runRefoldWith(t, env, "ci/d1")
	assert.Equal(t, 2, code)
	assert.Contains(t, errOut, "HOROPTER_ADMIN_KEY")
	assert.Empty(t, f.bodies, "no request may be made")
}

func TestRefoldUsageNeverPrintsTheAdminKey(t *testing.T) {
	code, _, errOut := runRefoldWith(t, adminEnv("http://x"), "--bogus")
	assert.Equal(t, 2, code)
	assert.Contains(t, errOut, "-key")
	assert.NotContains(t, errOut, "horo_ADMIN")
}

func TestRefoldNeedsExactlyOneRef(t *testing.T) {
	for _, args := range [][]string{{}, {"ci/a", "ci/b"}} {
		code, _, errOut := runRefoldWith(t, adminEnv("http://x"), args...)
		assert.Equal(t, 2, code, "%v", args)
		assert.Contains(t, errOut, "exactly one ref")
	}
}

// TestARouteMissingFromIngressIsNotARefNotFound: an ingress built before the route existed, or
// a URL pointing at egress, answers the mux's own 404. Exit 3 means "the ref has no events",
// so that must exit 1, or a script reads a misconfiguration as a missing change.
func TestARouteMissingFromIngressIsNotARefNotFound(t *testing.T) {
	f := &fakeRefold{status: http.StatusNotFound, answer: "404 page not found\n"}
	code, _, errOut := runRefoldWith(t, adminEnv(f.server(t)), "ci/d1")
	assert.Equal(t, 1, code)
	assert.Contains(t, errOut, "404")
}

func TestAnUnknownTenantIsNotARefNotFound(t *testing.T) {
	f := &fakeRefold{status: http.StatusNotFound, answer: `{"error":"no tenant \"acme\""}`}
	code, _, _ := runRefoldWith(t, adminEnv(f.server(t)), "--tenant", "acme", "ci/d1")
	assert.Equal(t, 1, code)
}
