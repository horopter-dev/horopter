package egress_test

import (
	"go/ast"
	"go/parser"
	"go/token"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/yo61/horopter/internal/egress"
)

// testLimits are the defaults with a short admission wait, so a shed read answers quickly.
func testLimits() egress.Limits {
	limits := egress.DefaultLimits()
	limits.AdmissionWait = 50 * time.Millisecond
	return limits
}

func changesURL(tn string) string {
	return "/v1/changes?tenant=" + tn + "&from=2026-09-14T09:00:00Z&to=2026-09-14T11:00:00Z"
}

// holdRead starts a GET of url on h and returns once that read holds its concurrency permit,
// with a function that lets it finish and returns its response. Only the first read admitted
// after the call is held; later ones pass the hook straight through. The read is let go when
// the test ends if the test has not done so.
func holdRead(t *testing.T, h http.Handler, url string) func() *httptest.ResponseRecorder {
	t.Helper()
	held, release := make(chan struct{}), make(chan struct{})
	var taken atomic.Bool
	egress.SetAfterAdmissionHook(func() {
		if taken.CompareAndSwap(false, true) {
			close(held)
			<-release
		}
	})
	t.Cleanup(func() { egress.SetAfterAdmissionHook(nil) })

	done := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, url, nil))
		done <- rec
	}()
	<-held

	var once sync.Once
	var rec *httptest.ResponseRecorder
	finish := func() *httptest.ResponseRecorder {
		once.Do(func() {
			close(release)
			rec = <-done
		})
		return rec
	}
	t.Cleanup(func() { finish() })
	return finish
}

func recordsWithMessage(records []map[string]any, msg string) []map[string]any {
	var out []map[string]any
	for _, r := range records {
		if r["msg"] == msg {
			out = append(out, r)
		}
	}
	return out
}

// TestAReadOverTheConcurrencyLimitIsShed fills a one-connection server's limit of one with a
// held read, so the next read can only be shed.
func TestAReadOverTheConcurrencyLimitIsShed(t *testing.T) {
	rec, restore := recordSpans(t)
	defer restore()
	srv, _, tn := newServerWith(t, 1, testLimits())
	h := srv.Routes()
	records := captureLogs(t)
	finish := holdRead(t, h, changesURL(tn))

	shed := get(t, h, changesURL(tn))
	held := finish()

	require.Equal(t, http.StatusOK, held.Code, held.Body.String())
	require.Equal(t, http.StatusServiceUnavailable, shed.Code, shed.Body.String())
	assert.Equal(t, "1", shed.Header().Get("Retry-After"))
	assert.JSONEq(t,
		`{"error":"egress is at capacity; retry after the interval in Retry-After"}`,
		shed.Body.String())

	warnings := recordsWithMessage(records(), "read shed")
	require.Len(t, warnings, 1, "every shed read is logged, once; got %v", records())
	w := warnings[0]
	assert.Equal(t, "WARN", w["level"])
	assert.Equal(t, "GET /v1/changes", w["route"])
	assert.Equal(t, tn, w["tenant"])
	assert.Equal(t, "concurrency", w["gate"])
	assert.Equal(t, float64(1), w["retry_after_s"])
	assert.Equal(t, float64(1), w["limit"])
	assert.Equal(t, float64(1), w["in_flight"])
	assert.GreaterOrEqual(t, w["waited_ms"], float64(40), "the read waited before it was shed")

	assert.ElementsMatch(t, []string{"admitted", "shed_concurrency"},
		spanAttrValues(rec, "", "horopter.admission.outcome"))
}

func TestProbesPassNoGate(t *testing.T) {
	srv, _, tn := newServerWith(t, 1, testLimits())
	h := srv.Routes()
	holdRead(t, h, changesURL(tn))
	require.Equal(t, http.StatusServiceUnavailable, get(t, h, changesURL(tn)).Code,
		"the concurrency limit must be full, or this test proves nothing")

	assert.Equal(t, http.StatusOK, get(t, h, "/healthz").Code)
	assert.Equal(t, http.StatusOK, get(t, h, "/readyz").Code)
}

// TestEveryReadRouteIsGated reads Routes' source: every route but the probes must be registered
// through s.admit, so a read route added later cannot bypass admission unnoticed.
func TestEveryReadRouteIsGated(t *testing.T) {
	gated, ungated := routeGating(t)

	assert.ElementsMatch(t, []string{"GET /healthz", "GET /readyz"}, ungated,
		"only the probes may bypass admission")
	var reads []string
	for _, route := range readRoutes("any") {
		reads = append(reads, route.pattern)
	}
	assert.ElementsMatch(t, reads, gated)
}

// routeGating splits the patterns Routes registers into those whose handler is a call to
// s.admit and those whose handler is not.
func routeGating(t *testing.T) (gated, ungated []string) {
	t.Helper()
	file, err := parser.ParseFile(token.NewFileSet(), "server.go", nil,
		parser.SkipObjectResolution)
	require.NoError(t, err)
	ast.Inspect(file, func(n ast.Node) bool {
		fn, ok := n.(*ast.FuncDecl)
		if !ok || fn.Name.Name != "Routes" {
			return true
		}
		ast.Inspect(fn.Body, func(inner ast.Node) bool {
			call, ok := inner.(*ast.CallExpr)
			if !ok {
				return true
			}
			if pattern, matched := routeHandleCall(t, call); matched {
				if isAdmitCall(call.Args[len(call.Args)-1]) {
					gated = append(gated, pattern)
				} else {
					ungated = append(ungated, pattern)
				}
			}
			return true
		})
		return false
	})
	return gated, ungated
}

// isAdmitCall reports whether e is a call to a method named admit.
func isAdmitCall(e ast.Expr) bool {
	call, ok := e.(*ast.CallExpr)
	if !ok {
		return false
	}
	sel, ok := call.Fun.(*ast.SelectorExpr)
	return ok && sel.Sel.Name == "admit"
}

func TestRetryAfterRoundsTheAdmissionWaitUpToWholeSeconds(t *testing.T) {
	for wait, want := range map[time.Duration]int{
		time.Millisecond:        1,
		500 * time.Millisecond:  1,
		time.Second:             1,
		1001 * time.Millisecond: 2,
		10 * time.Second:        10,
	} {
		assert.Equal(t, want, egress.RetryAfterSeconds(wait), "wait %s", wait)
	}
}
