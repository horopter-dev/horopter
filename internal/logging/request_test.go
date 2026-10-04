package logging

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// serve runs req through Requests(mux) with slog's default sent to a buffer, and returns
// every record written. No test in this package calls t.Parallel, which is what makes
// swapping the process-wide default safe.
func serve(t *testing.T, mux http.Handler, req *http.Request) []map[string]any {
	t.Helper()
	var buf bytes.Buffer
	previous := slog.Default()
	slog.SetDefault(slog.New(handler(&buf, slog.LevelDebug)))
	t.Cleanup(func() { slog.SetDefault(previous) })
	Requests(mux).ServeHTTP(httptest.NewRecorder(), req)
	var records []map[string]any
	dec := json.NewDecoder(&buf)
	for dec.More() {
		var record map[string]any
		require.NoError(t, dec.Decode(&record))
		records = append(records, record)
	}
	return records
}

func muxWith(pattern string, h http.HandlerFunc) *http.ServeMux {
	m := http.NewServeMux()
	m.HandleFunc(pattern, h)
	return m
}

func TestOneLinePerRequestWithRouteStatusAndBytes(t *testing.T) {
	m := muxWith("POST /v1/events", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte("hello"))
	})
	records := serve(t, m, httptest.NewRequest(http.MethodPost, "/v1/events", nil))
	require.Len(t, records, 1)
	line := records[0]
	assert.Equal(t, "request", line["msg"])
	assert.Equal(t, "INFO", line["level"])
	assert.Equal(t, "POST", line["method"])
	assert.Equal(t, "POST /v1/events", line["route"], "the pattern the mux matched")
	assert.InDelta(t, 201, line["status"], 0)
	assert.InDelta(t, 5, line["bytes"], 0)
	assert.Contains(t, line, "duration_ms")
}

func TestAHandlerThatWritesNothingLogs200(t *testing.T) {
	m := muxWith("GET /v1/changes", func(http.ResponseWriter, *http.Request) {})
	records := serve(t, m, httptest.NewRequest(http.MethodGet, "/v1/changes", nil))
	require.Len(t, records, 1)
	assert.InDelta(t, 200, records[0]["status"], 0)
}

func TestTheProbesWriteNoLine(t *testing.T) {
	for _, path := range []string{"/healthz", "/readyz"} {
		m := muxWith("GET "+path, func(http.ResponseWriter, *http.Request) {})
		assert.Empty(t, serve(t, m, httptest.NewRequest(http.MethodGet, path, nil)), path)
	}
}

func TestAnUnmatchedPathLogsUnmatched(t *testing.T) {
	m := muxWith("GET /v1/changes", func(http.ResponseWriter, *http.Request) {})
	records := serve(t, m, httptest.NewRequest(http.MethodGet, "/nope", nil))
	require.Len(t, records, 1)
	assert.Equal(t, "unmatched", records[0]["route"])
	assert.InDelta(t, 404, records[0]["status"], 0)
}

func TestAPanicStillLogs500AndRepanics(t *testing.T) {
	var buf bytes.Buffer
	previous := slog.Default()
	slog.SetDefault(slog.New(handler(&buf, slog.LevelDebug)))
	t.Cleanup(func() { slog.SetDefault(previous) })
	m := muxWith("GET /boom", func(http.ResponseWriter, *http.Request) { panic("boom") })

	assert.PanicsWithValue(t, "boom", func() {
		Requests(m).ServeHTTP(httptest.NewRecorder(),
			httptest.NewRequest(http.MethodGet, "/boom", nil))
	})
	var line map[string]any
	require.NoError(t, json.Unmarshal(buf.Bytes(), &line), "wrote %q", buf.String())
	assert.InDelta(t, 500, line["status"], 0)
	assert.Equal(t, "GET /boom", line["route"])
}

func TestTraceIdsArePresentWhenASpanIsInTheContext(t *testing.T) {
	ctx, traceID, spanID := spanContext(t)
	m := muxWith("GET /v1/changes", func(http.ResponseWriter, *http.Request) {})
	req := httptest.NewRequest(http.MethodGet, "/v1/changes", nil).WithContext(ctx)
	records := serve(t, m, req)
	require.Len(t, records, 1)
	assert.Equal(t, traceID, records[0]["trace_id"])
	assert.Equal(t, spanID, records[0]["span_id"])
}

func TestAFlushingHandlerReachesTheFlusherThroughTheWrapper(t *testing.T) {
	var flushErr error
	m := muxWith("GET /stream", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("a"))
		flushErr = http.NewResponseController(w).Flush()
	})
	serve(t, m, httptest.NewRequest(http.MethodGet, "/stream", nil))
	assert.NoError(t, flushErr)
}
