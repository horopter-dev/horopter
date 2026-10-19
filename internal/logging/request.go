package logging

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"sync"
	"time"
	"unicode/utf8"
)

// unlogged are the probe routes, which kubelet calls every few seconds.
var unlogged = map[string]bool{"GET /healthz": true, "GET /readyz": true}

type recordKey struct{}

type requestConfig struct{ query bool }

// RequestOption configures Requests.
type RequestOption func(*requestConfig)

// Requests logs one line per request; it wraps the mux, inside otelhttp.
func Requests(next http.Handler, opts ...RequestOption) http.Handler {
	var cfg requestConfig
	for _, opt := range opts {
		opt(&cfg)
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rec := &requestRecord{}
		// ServeMux sets Pattern on the request it is given, so the route is read from routed.
		routed := r.WithContext(context.WithValue(r.Context(), recordKey{}, rec))
		rw := &responseRecorder{ResponseWriter: w}
		start := time.Now()
		defer func() {
			p := recover()
			if !unlogged[routed.Pattern] {
				status := rw.statusOr200()
				if p != nil {
					status = http.StatusInternalServerError
				}
				writeLine(routed, rec, rw, status, time.Since(start), cfg)
			}
			if p != nil {
				panic(p)
			}
		}()
		next.ServeHTTP(rw, routed)
	})
}

const (
	// maxField bounds every string on the line: Loki rejects an over-long line outright.
	maxField = 1 << 10
	// maxErrorBody fits a message quoting ingress's 1 MiB body cap, plus JSON escaping.
	maxErrorBody = 2 << 20
)

// WithQuery logs the raw query string of a request whose credential resolved.
func WithQuery() RequestOption { return func(c *requestConfig) { c.query = true } }

// Annotate adds fields to the current request's line; outside Requests it does nothing.
func Annotate(ctx context.Context, attrs ...slog.Attr) {
	rec, ok := ctx.Value(recordKey{}).(*requestRecord)
	if !ok {
		return
	}
	rec.mu.Lock()
	defer rec.mu.Unlock()
	rec.attrs = append(rec.attrs, attrs...)
}

// requestRecord collects the fields Annotate adds during a request.
type requestRecord struct {
	mu    sync.Mutex
	attrs []slog.Attr
}

// snapshot returns the annotations, the last value of each key in first-seen order.
func (r *requestRecord) snapshot() []slog.Attr {
	r.mu.Lock()
	defer r.mu.Unlock()
	index := make(map[string]int, len(r.attrs))
	var out []slog.Attr
	for _, a := range r.attrs {
		if i, seen := index[a.Key]; seen {
			out[i] = a
			continue
		}
		index[a.Key] = len(out)
		out = append(out, a)
	}
	return out
}

func writeLine(
	r *http.Request, rec *requestRecord, rw *responseRecorder, status int, d time.Duration,
	cfg requestConfig,
) {
	route := r.Pattern
	if route == "" {
		route = "unmatched"
	}
	attrs := []slog.Attr{
		slog.String("method", r.Method),
		slog.String("route", route),
		slog.Int("status", status),
		slog.Float64("duration_ms", float64(d.Microseconds())/1000),
		slog.Int("bytes", rw.bytes),
	}
	annotated := rec.snapshot()
	attrs = append(attrs, annotated...)
	if cfg.query && r.URL.RawQuery != "" && hasKey(annotated, "credential") {
		attrs = append(attrs, slog.String("query", r.URL.RawQuery))
	}
	attrs = append(attrs, errorAttrs(rw, status)...)
	slog.LogAttrs(r.Context(), slog.LevelInfo, "request", cut(attrs)...)
}

func hasKey(attrs []slog.Attr, key string) bool {
	for _, a := range attrs {
		if a.Key == key {
			return true
		}
	}
	return false
}

// errorAttrs reads the "error" field every daemon's writeError puts in an error body.
func errorAttrs(rw *responseRecorder, status int) []slog.Attr {
	if status < http.StatusBadRequest {
		return nil
	}
	if rw.overflow {
		return []slog.Attr{slog.Bool("error_truncated", true)}
	}
	var body struct {
		Error *string `json:"error"`
	}
	if json.Unmarshal(rw.body, &body) != nil || body.Error == nil {
		return nil
	}
	return []slog.Attr{slog.String("error", *body.Error)}
}

// cut bounds every string attribute at maxField, flagging each one it shortened.
func cut(attrs []slog.Attr) []slog.Attr {
	out := make([]slog.Attr, 0, len(attrs))
	for _, a := range attrs {
		s := a.Value.String()
		if a.Value.Kind() != slog.KindString || len(s) <= maxField {
			out = append(out, a)
			continue
		}
		out = append(out, slog.String(a.Key, validPrefix(s, maxField)),
			slog.Bool(a.Key+"_truncated", true))
	}
	return out
}

// validPrefix returns at most n bytes of s, backing off to a rune start at the cut.
func validPrefix(s string, n int) string {
	for back := 0; back < utf8.UTFMax && n > 0 && !utf8.RuneStart(s[n]); back++ {
		n--
	}
	return s[:n]
}

// responseRecorder captures what the handler wrote, and retains an error response's body.
type responseRecorder struct {
	http.ResponseWriter
	status   int
	bytes    int
	body     []byte
	overflow bool
}

func (r *responseRecorder) WriteHeader(code int) {
	if r.status == 0 {
		r.status = code
	}
	r.ResponseWriter.WriteHeader(code)
}

func (r *responseRecorder) Write(b []byte) (int, error) {
	if r.status == 0 {
		r.status = http.StatusOK
	}
	n, err := r.ResponseWriter.Write(b)
	r.bytes += n
	if r.status >= http.StatusBadRequest {
		r.retain(b[:n])
	}
	return n, err
}

func (r *responseRecorder) retain(b []byte) {
	if r.overflow {
		return
	}
	if len(r.body)+len(b) > maxErrorBody {
		r.overflow, r.body = true, nil
		return
	}
	r.body = append(r.body, b...)
}

// Unwrap lets http.ResponseController reach the Flusher beneath.
func (r *responseRecorder) Unwrap() http.ResponseWriter { return r.ResponseWriter }

func (r *responseRecorder) statusOr200() int {
	if r.status == 0 {
		return http.StatusOK
	}
	return r.status
}
