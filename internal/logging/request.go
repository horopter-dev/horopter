package logging

import (
	"context"
	"log/slog"
	"net/http"
	"time"
)

// unlogged are the probe routes. Kubelet calls them every few seconds on both daemons, and
// a failing readyz already logs its own error.
var unlogged = map[string]bool{"GET /healthz": true, "GET /readyz": true}

// Requests writes one line per request handled by next.
//
// It belongs inside otelhttp, so the span is in the context and traceHandler adds its ids,
// and around the mux, so the matched pattern is known when next returns. ServeMux writes
// the pattern into the request it is given, and that is the copy carrying the record, not
// r: so the route is read from routed.
func Requests(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		routed := r.WithContext(context.WithValue(r.Context(), recordKey{}, &requestRecord{}))
		rw := &responseRecorder{ResponseWriter: w}
		start := time.Now()
		defer func() {
			p := recover()
			if !unlogged[routed.Pattern] {
				status := rw.statusOr200()
				if p != nil {
					status = http.StatusInternalServerError
				}
				writeLine(routed, rw, status, time.Since(start))
			}
			if p != nil {
				panic(p)
			}
		}()
		next.ServeHTTP(rw, routed)
	})
}

func writeLine(r *http.Request, rw *responseRecorder, status int, d time.Duration) {
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
	slog.LogAttrs(r.Context(), slog.LevelInfo, "request", attrs...)
}

// requestRecord will collect the fields Annotate adds; Task 2 gives it a body.
type requestRecord struct{}

type recordKey struct{}

// responseRecorder captures what the handler wrote.
type responseRecorder struct {
	http.ResponseWriter
	status int
	bytes  int
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
	return n, err
}

// Unwrap lets http.ResponseController reach the Flusher beneath.
func (r *responseRecorder) Unwrap() http.ResponseWriter { return r.ResponseWriter }

func (r *responseRecorder) statusOr200() int {
	if r.status == 0 {
		return http.StatusOK
	}
	return r.status
}
