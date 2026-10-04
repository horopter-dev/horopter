package logging

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"unicode/utf8"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// serve runs req through Requests(mux) with slog's default sent to a buffer, and returns
// every record written. No test in this package calls t.Parallel, which is what makes
// swapping the process-wide default safe.
func serve(
	t *testing.T, mux http.Handler, req *http.Request, opts ...RequestOption,
) []map[string]any {
	t.Helper()
	var buf bytes.Buffer
	previous := slog.Default()
	slog.SetDefault(slog.New(Handler(&buf, slog.LevelDebug)))
	t.Cleanup(func() { slog.SetDefault(previous) })
	Requests(mux, opts...).ServeHTTP(httptest.NewRecorder(), req)
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
	slog.SetDefault(slog.New(Handler(&buf, slog.LevelDebug)))
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

func TestAnnotateWithoutARecordDoesNothing(t *testing.T) {
	assert.NotPanics(t, func() { Annotate(context.Background(), slog.String("k", "v")) })
}

func TestAnnotationsReachTheLineAndTheLastValueWins(t *testing.T) {
	m := muxWith("POST /v1/events", func(_ http.ResponseWriter, r *http.Request) {
		Annotate(r.Context(), slog.String("subject", "first"), slog.String("type", "t"))
		Annotate(r.Context(), slog.String("subject", "acme/prod"))
	})
	records := serve(t, m, httptest.NewRequest(http.MethodPost, "/v1/events", nil))
	require.Len(t, records, 1)
	assert.Equal(t, "acme/prod", records[0]["subject"])
	assert.Equal(t, "t", records[0]["type"])
}

func TestConcurrentAnnotationsAreSafe(t *testing.T) {
	m := muxWith("POST /v1/events", func(_ http.ResponseWriter, r *http.Request) {
		var wg sync.WaitGroup
		for i := range 50 {
			wg.Go(func() { Annotate(r.Context(), slog.Int("n", i)) })
		}
		wg.Wait()
	})
	records := serve(t, m, httptest.NewRequest(http.MethodPost, "/v1/events", nil))
	require.Len(t, records, 1)
	assert.Contains(t, records[0], "n")
}

func TestErrorIsReadFromAJSONErrorBody(t *testing.T) {
	m := muxWith("POST /v1/events", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"error":"subject is required"}`))
	})
	records := serve(t, m, httptest.NewRequest(http.MethodPost, "/v1/events", nil))
	assert.Equal(t, "subject is required", records[0]["error"])
}

func TestANonJSONErrorBodyAddsNoErrorField(t *testing.T) {
	m := muxWith("POST /v1/events", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte("plain text"))
	})
	records := serve(t, m, httptest.NewRequest(http.MethodPost, "/v1/events", nil))
	assert.NotContains(t, records[0], "error")
	assert.NotContains(t, records[0], "error_truncated")
}

func TestALongErrorIsCutToOneKiBAndFlagged(t *testing.T) {
	msg := strings.Repeat("x", 8<<10)
	m := muxWith("POST /v1/events", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		_ = json.NewEncoder(w).Encode(map[string]string{"error": msg})
	})
	records := serve(t, m, httptest.NewRequest(http.MethodPost, "/v1/events", nil))
	assert.Len(t, records[0]["error"], 1024)
	assert.Equal(t, true, records[0]["error_truncated"])
}

func TestAnErrorBodyPastTheRetentionBoundIsFlaggedWithoutText(t *testing.T) {
	msg := strings.Repeat("x", 3<<20)
	m := muxWith("POST /v1/events", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		_ = json.NewEncoder(w).Encode(map[string]string{"error": msg})
	})
	records := serve(t, m, httptest.NewRequest(http.MethodPost, "/v1/events", nil))
	assert.NotContains(t, records[0], "error")
	assert.Equal(t, true, records[0]["error_truncated"])
}

func TestEveryStringFieldIsCutAtOneKiB(t *testing.T) {
	m := muxWith("POST /v1/events", func(_ http.ResponseWriter, r *http.Request) {
		Annotate(r.Context(), slog.String("subject", strings.Repeat("s", 4<<10)))
	})
	records := serve(t, m, httptest.NewRequest(http.MethodPost, "/v1/events", nil))
	assert.Len(t, records[0]["subject"], 1024)
	assert.Equal(t, true, records[0]["subject_truncated"])
}

func TestACutNeverSplitsAMultiByteCharacter(t *testing.T) {
	// "é" is two bytes; 1023 bytes of "a" put its first byte at offset 1023.
	value := strings.Repeat("a", 1023) + strings.Repeat("é", 10)
	m := muxWith("POST /v1/events", func(_ http.ResponseWriter, r *http.Request) {
		Annotate(r.Context(), slog.String("subject", value))
	})
	records := serve(t, m, httptest.NewRequest(http.MethodPost, "/v1/events", nil))
	got, ok := records[0]["subject"].(string)
	require.True(t, ok)
	assert.True(t, utf8.ValidString(got))
	assert.Equal(t, strings.Repeat("a", 1023), got)
}

func TestTheQueryIsLoggedOnlyWithACredential(t *testing.T) {
	anonymous := muxWith("GET /v1/changes", func(http.ResponseWriter, *http.Request) {})
	records := serve(t, anonymous,
		httptest.NewRequest(http.MethodGet, "/v1/changes?ref=ci/1", nil), WithQuery())
	assert.NotContains(t, records[0], "query")

	known := muxWith("GET /v1/changes", func(_ http.ResponseWriter, r *http.Request) {
		Annotate(r.Context(), slog.String("credential", "0123456789ab"))
	})
	records = serve(t, known,
		httptest.NewRequest(http.MethodGet, "/v1/changes?ref=ci/1", nil), WithQuery())
	assert.Equal(t, "ref=ci/1", records[0]["query"])
}

func TestTheQueryIsNeverLoggedWithoutTheOption(t *testing.T) {
	m := muxWith("GET /v1/changes", func(_ http.ResponseWriter, r *http.Request) {
		Annotate(r.Context(), slog.String("credential", "0123456789ab"))
	})
	records := serve(t, m, httptest.NewRequest(http.MethodGet, "/v1/changes?ref=ci/1", nil))
	assert.NotContains(t, records[0], "query")
}

// TestAnInvalidByteDoesNotEmptyALongField: validPrefix used to trim until the whole prefix
// was valid UTF-8, so one bad byte near the start of a long field cut it to nothing.
func TestAnInvalidByteDoesNotEmptyALongField(t *testing.T) {
	value := "a\xff" + strings.Repeat("b", 4<<10)
	m := muxWith("POST /v1/events", func(_ http.ResponseWriter, r *http.Request) {
		Annotate(r.Context(), slog.String("subject", value))
	})
	records := serve(t, m, httptest.NewRequest(http.MethodPost, "/v1/events", nil))
	got, ok := records[0]["subject"].(string)
	require.True(t, ok)
	assert.Greater(t, len(got), 1000, "the field kept its content, not just its flag")
	assert.Equal(t, true, records[0]["subject_truncated"])
}
