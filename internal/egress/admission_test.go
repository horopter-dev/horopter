package egress_test

import (
	"context"
	"errors"
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
	"horopter.dev/horopter/internal/auth"
	"horopter.dev/horopter/internal/egress"
	"horopter.dev/horopter/internal/store/postgres"
)

func testLimits() egress.Limits {
	limits := egress.DefaultLimits()
	limits.AdmissionWait = 50 * time.Millisecond
	return limits
}

func changesURL(tn string) string {
	return "/v1/changes?tenant=" + tn + "&from=2026-09-14T09:00:00Z&to=2026-09-14T11:00:00Z"
}

// Warms auth on a throwaway server: the first lookup would itself raise this limiter.
func newServerWithWarmedAuth(
	t *testing.T, poolMaxConns int, limits egress.Limits,
) (srv *egress.Server, s *postgres.Store, tn, key string) {
	t.Helper()
	s, tn = openTestStore(t, poolMaxConns)
	authn := auth.New(auth.NewStore(s.Pool()), fixedClock, auth.DefaultOptions())
	key = readerKey(t, s, tn)

	warmup, err := egress.New(s, fixedClock, egress.DefaultLimits(), authn)
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, get(t, warmup.Routes(), key, changesURL(tn)).Code)

	srv, err = egress.New(s, fixedClock, limits, authn)
	require.NoError(t, err)
	return srv, s, tn, key
}

// Holds only the first read admitted after the call; later ones pass straight through.
func holdRead(t *testing.T, h http.Handler, key, url string) func() *httptest.ResponseRecorder {
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
		req := httptest.NewRequest(http.MethodGet, url, nil)
		req.Header.Set("Authorization", "Bearer "+key)
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
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

func TestAReadOverTheConcurrencyLimitIsShed(t *testing.T) {
	rec, restore := recordSpans(t)
	defer restore()
	srv, s, tn := newServerWith(t, 1, testLimits())
	h := srv.Routes()
	key := readerKey(t, s, tn)
	records := captureLogs(t)
	finish := holdRead(t, h, key, changesURL(tn))

	shed := get(t, h, key, changesURL(tn))
	held := finish()

	require.Equal(t, http.StatusOK, held.Code, held.Body.String())

	after := get(t, h, key, changesURL(tn))
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
	assert.Len(t, spanAttrValues(rec, "", "horopter.db.connection_wait_ms"), 2,
		"the held read and the follow-up read acquired a connection and so carry the "+
			"connection-wait attribute; the shed read never acquired one and must not")
}

// recordSpans first: otelpgx binds its tracer to the global provider at pool construction.
func TestConcurrencyShedFromAGlobalCredentialDoesNotQueryTenantExists(t *testing.T) {
	rec, restore := recordSpans(t)
	defer restore()

	srv, s, tn := newServerWith(t, 1, testLimits())
	h := srv.Routes()
	key := readerKey(t, s, tn)
	superuserKey := seedCredential(t, s, "superuser", "")

	finish := holdRead(t, h, key, changesURL(tn))

	shed := get(t, h, superuserKey, changesURL(tn))
	require.Equal(t, http.StatusServiceUnavailable, shed.Code, shed.Body.String())

	held := finish()
	require.Equal(t, http.StatusOK, held.Code, held.Body.String())

	queries := spanAttrValues(rec, "", "db.query.text")
	require.True(t, slices.ContainsFunc(queries, func(text string) bool {
		return strings.Contains(text, "FROM change")
	}), "positive control: the held read's own query must appear in the recorded spans, or "+
		"an absent TenantExists span proves nothing — tracing itself could be the thing "+
		"that silently stopped working; got %v", queries)
	for _, text := range queries {
		assert.NotContains(t, text, "FROM tenant WHERE id",
			"a request shed at the concurrency gate must never reach tenantFor's own "+
				"database round trip: it holds no permit, and TenantExists is exactly the "+
				"load admission exists to bound")
	}
}

func TestProbesPassNoGate(t *testing.T) {
	srv, s, tn := newServerWith(t, 1, testLimits())
	h := srv.Routes()
	key := readerKey(t, s, tn)
	release, err := egress.ReserveReadMemory(srv, testLimits().ReadMemoryBytes)
	require.NoError(t, err)
	t.Cleanup(release)
	holdRead(t, h, key, changesURL(tn))
	require.Equal(t, http.StatusServiceUnavailable, get(t, h, key, changesURL(tn)).Code,
		"the concurrency limit must be full, or this test proves nothing")

	assert.Equal(t, http.StatusOK, getUnauthenticated(t, h, "GET /healthz").Code)
	assert.Equal(t, http.StatusOK, getUnauthenticated(t, h, "GET /readyz").Code)
}

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

// Routes wraps gated handlers as s.authz(cap, s.admit(h)).
func isAdmitCall(e ast.Expr) bool {
	call, ok := e.(*ast.CallExpr)
	if !ok {
		return false
	}
	sel, ok := call.Fun.(*ast.SelectorExpr)
	if !ok {
		return false
	}
	if sel.Sel.Name == "admit" {
		return true
	}
	return sel.Sel.Name == "authz" && len(call.Args) > 0 &&
		isAdmitCall(call.Args[len(call.Args)-1])
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

func TestAReadThatWaitsForAConnectionHalvesTheLimit(t *testing.T) {
	limits := egress.DefaultLimits()
	limits.DBWaitThreshold = 50 * time.Millisecond
	srv, s, tn, key := newServerWithWarmedAuth(t, 2, limits)
	h := srv.Routes()

	rec, restore := recordSpans(t)
	defer restore()
	records := captureLogs(t)
	require.Equal(t, 2, egress.ReadConcurrencyLimit(srv), "the limit starts at the pool size")

	release := holdConnections(t, s, 2)
	go func() {
		time.Sleep(200 * time.Millisecond)
		release()
	}()
	require.Equal(t, http.StatusOK, get(t, h, key, changesURL(tn)).Code)

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

	require.Equal(t, http.StatusOK, get(t, h, key, changesURL(tn)).Code)
	assert.Equal(t, 2, egress.ReadConcurrencyLimit(srv), "a healthy read raises the limit again")
}

// A client giving up says nothing about the database.
func TestAReadWhoseClientGivesUpDoesNotLowerTheLimit(t *testing.T) {
	limits := egress.DefaultLimits()
	limits.DBWaitThreshold = 500 * time.Millisecond
	srv, s, tn := newServerWith(t, 2, limits)
	h := srv.Routes()
	key := readerKey(t, s, tn)
	// Warmed first: an uncached lookup would wait on the held connections.
	require.Equal(t, http.StatusOK, get(t, h, key, changesURL(tn)).Code)
	holdConnections(t, s, 2)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()

	req := httptest.NewRequest(http.MethodGet, changesURL(tn), nil).WithContext(ctx)
	req.Header.Set("Authorization", "Bearer "+key)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	require.Equal(t, http.StatusInternalServerError, rec.Code,
		"the read must have failed to get a connection, or this test proves nothing")
	assert.GreaterOrEqual(t, egress.ReadConcurrencyLimit(srv), 2)
}

// A second read must not log another decrease: the limit is already at its floor.
func TestAFailedConnectionAcquisitionHalvesTheLimit(t *testing.T) {
	srv, s, tn, key := newServerWithWarmedAuth(t, 2, egress.DefaultLimits())
	h := srv.Routes()
	records := captureLogs(t)
	s.Close()

	require.Equal(t, http.StatusInternalServerError, get(t, h, key, changesURL(tn)).Code)
	require.Equal(t, http.StatusInternalServerError, get(t, h, key, changesURL(tn)).Code)

	assert.Equal(t, 1, egress.ReadConcurrencyLimit(srv))
	var halved []map[string]any
	for _, r := range records() {
		if msg, _ := r["msg"].(string); strings.HasPrefix(msg, "read concurrency limit halved") {
			halved = append(halved, r)
		}
	}
	require.Len(t, halved, 1, "the limit is already at its floor after the first decrease, so "+
		"a second congested read must not log a second one; got %v", records())
	assert.Equal(t, "WARN", halved[0]["level"])
	assert.Equal(t, "read concurrency limit halved from 2 to 1 after a failed database "+
		"connection acquisition", halved[0]["msg"])
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

// The wait counts against threshold even when the client disconnected.
func TestAClientThatGaveUpAfterALongWaitStillCountsAsOverloaded(t *testing.T) {
	threshold := 100 * time.Millisecond
	cancelled, cancel := context.WithCancel(context.Background())
	cancel()

	assert.True(t, egress.ReadOverloadedByAcquisition(
		cancelled, threshold, 150*time.Millisecond, context.Canceled),
		"a long wait must count even though the client had already given up")
	assert.False(t, egress.ReadOverloadedByAcquisition(
		cancelled, threshold, 10*time.Millisecond, context.Canceled),
		"a short wait under a gone client is not overloaded")
	assert.True(t, egress.ReadOverloadedByAcquisition(
		context.Background(), threshold, 10*time.Millisecond, errors.New("pool exhausted")),
		"a failed acquisition on a live request context is overloaded however briefly it waited")
}
