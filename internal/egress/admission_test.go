package egress_test

import (
	"context"
	"go/ast"
	"go/parser"
	"go/token"
	"net/http"
	"net/http/httptest"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/yo61/horopter/internal/egress"
	"github.com/yo61/horopter/internal/store/postgres"
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
	select {
	case <-held:
	case rec := <-done:
		t.Fatalf("read finished without reaching the admission hook: status %d, body %q",
			rec.Code, rec.Body.String())
	case <-time.After(5 * time.Second):
		t.Fatal("read never reached the admission hook")
	}

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

	after := get(t, h, changesURL(tn))
	assert.Equal(t, http.StatusOK, after.Code,
		"the held read must have given its permit back: %s", after.Body.String())

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

	assert.ElementsMatch(t, []string{"admitted", "admitted", "shed_concurrency"},
		spanAttrValues(rec, "", "horopter.admission.outcome"),
		"the held read, the shed read and the post-release check each contribute one outcome")
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

// holdConnections takes n of the store's pool connections outside any read, and returns a
// function giving them back. They are given back when the test ends if not before.
func holdConnections(t *testing.T, s *postgres.Store, n int) func() {
	t.Helper()
	conns := make([]*pgxpool.Conn, 0, n)
	for range n {
		conn, err := s.Pool().Acquire(context.Background())
		require.NoError(t, err)
		conns = append(conns, conn)
	}
	var once sync.Once
	release := func() {
		once.Do(func() {
			for _, conn := range conns {
				conn.Release()
			}
		})
	}
	t.Cleanup(release)
	return release
}

// TestAReadThatWaitsForAConnectionHalvesTheLimit holds a two-connection pool's connections
// while a read starts, so the read waits past the threshold before it gets one.
func TestAReadThatWaitsForAConnectionHalvesTheLimit(t *testing.T) {
	rec, restore := recordSpans(t)
	defer restore()
	limits := egress.DefaultLimits()
	limits.DBWaitThreshold = 50 * time.Millisecond
	srv, s, tn := newServerWith(t, 2, limits)
	h := srv.Routes()
	records := captureLogs(t)
	require.Equal(t, 2, egress.ReadConcurrencyLimit(srv), "the limit starts at the pool size")

	release := holdConnections(t, s, 2)
	go func() {
		time.Sleep(200 * time.Millisecond)
		release()
	}()
	require.Equal(t, http.StatusOK, get(t, h, changesURL(tn)).Code)

	assert.Equal(t, 1, egress.ReadConcurrencyLimit(srv))
	halved := slices.ContainsFunc(records(), func(r map[string]any) bool {
		msg, _ := r["msg"].(string)
		return r["level"] == "WARN" &&
			strings.HasPrefix(msg, "read concurrency limit halved from 2 to 1 after a ") &&
			strings.HasSuffix(msg, "ms database connection wait")
	})
	assert.True(t, halved, "a decrease is logged; got %v", records())
	waits := spanAttrValues(rec, "", "horopter.db.connection_wait_ms")
	require.Len(t, waits, 1)
	waited, err := strconv.Atoi(waits[0])
	require.NoError(t, err)
	assert.GreaterOrEqual(t, waited, 150)

	require.Equal(t, http.StatusOK, get(t, h, changesURL(tn)).Code)
	assert.Equal(t, 2, egress.ReadConcurrencyLimit(srv), "a healthy read raises the limit again")
}

// TestAReadWhoseClientGivesUpDoesNotLowerTheLimit ends the request's context while the read waits
// for a connection, briefly: a client giving up says nothing about the database.
func TestAReadWhoseClientGivesUpDoesNotLowerTheLimit(t *testing.T) {
	limits := egress.DefaultLimits()
	limits.DBWaitThreshold = 500 * time.Millisecond
	srv, s, tn := newServerWith(t, 2, limits)
	h := srv.Routes()
	holdConnections(t, s, 2)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, changesURL(tn), nil).WithContext(ctx))

	require.Equal(t, http.StatusInternalServerError, rec.Code,
		"the read must have failed to get a connection, or this test proves nothing")
	assert.GreaterOrEqual(t, egress.ReadConcurrencyLimit(srv), 2)
}

// TestAFailedConnectionAcquisitionHalvesTheLimit closes the pool under a live request: a pool
// that cannot hand out connections is overloaded, however briefly the read waited.
func TestAFailedConnectionAcquisitionHalvesTheLimit(t *testing.T) {
	srv, s, tn := newServerWith(t, 2, egress.DefaultLimits())
	h := srv.Routes()
	records := captureLogs(t)
	s.Close()

	require.Equal(t, http.StatusInternalServerError, get(t, h, changesURL(tn)).Code)

	assert.Equal(t, 1, egress.ReadConcurrencyLimit(srv))
	assert.True(t, slices.ContainsFunc(records(), func(r map[string]any) bool {
		return r["level"] == "WARN" && r["msg"] == "read concurrency limit halved from 2 to 1 "+
			"after a failed database connection acquisition"
	}), "got %v", records())
}

func TestOverloadIsDecidedByTheLongestConnectionWait(t *testing.T) {
	threshold := 100 * time.Millisecond

	assert.True(t, egress.ReadOverloaded(threshold, 150*time.Millisecond, 10*time.Millisecond))
	assert.True(t, egress.ReadOverloaded(threshold, 10*time.Millisecond, 150*time.Millisecond),
		"a long wait after a short one still counts")
	assert.False(t, egress.ReadOverloaded(threshold, 10*time.Millisecond, 100*time.Millisecond),
		"a wait at the threshold is not over it")
	assert.False(t, egress.ReadOverloaded(threshold),
		"a read that acquired nothing waited for nothing")
}
