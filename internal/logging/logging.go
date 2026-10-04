// Package logging installs the process-wide slog handler.
//
// The daemons run in containers whose output is shipped to a log store, so they emit JSON:
// every call site already passes key/value pairs, and the default text handler formatted
// that structure away. Records logged with a context also carry the active span's ids, which
// is what links a line to its trace.
package logging

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"strings"

	"go.opentelemetry.io/otel/trace"
)

// LevelVar names the environment variable holding the minimum level to log.
const LevelVar = "HOROPTER_LOG_LEVEL"

// Setup installs the JSON handler as slog's default.
//
// Call it before telemetry.Setup: that function hands slog's default handler to
// OpenTelemetry's internal logger, so the order decides whether OTel's own diagnostics are
// JSON or text.
func Setup() error {
	level, err := levelFromEnv()
	if err != nil {
		return err
	}
	slog.SetDefault(slog.New(Handler(os.Stderr, level)))
	return nil
}

// Handler is the handler Setup installs: JSON to w at level, with the active span's ids on
// records logged with a context. Exported so another package's tests can log exactly as
// the daemons do.
func Handler(w io.Writer, level slog.Leveler) slog.Handler {
	return traceHandler{Handler: slog.NewJSONHandler(w, &slog.HandlerOptions{Level: level})}
}

func levelFromEnv() (slog.Level, error) {
	raw := strings.TrimSpace(os.Getenv(LevelVar))
	switch strings.ToLower(raw) {
	case "":
		return slog.LevelInfo, nil
	case "debug":
		return slog.LevelDebug, nil
	case "info":
		return slog.LevelInfo, nil
	case "warn":
		return slog.LevelWarn, nil
	case "error":
		return slog.LevelError, nil
	}
	return 0, fmt.Errorf("%s=%q is not a level: use debug, info, warn or error", LevelVar, raw)
}

// traceHandler adds the active span's ids to records logged with a context.
//
// WithAttrs and WithGroup re-wrap what they delegate to. Returning the inner handler from
// either would silently unwrap the correlation: every logger built with attributes or a
// group would still log, just without ids, and nothing would fail.
type traceHandler struct{ slog.Handler }

func (h traceHandler) Handle(ctx context.Context, r slog.Record) error {
	sc := trace.SpanContextFromContext(ctx)
	if !sc.IsValid() {
		return h.Handler.Handle(ctx, r)
	}
	// Clone before adding: a Record's attributes may share a backing array with the
	// caller's, and a handler must not mutate what it was given.
	out := r.Clone()
	out.AddAttrs(
		slog.String("trace_id", sc.TraceID().String()),
		slog.String("span_id", sc.SpanID().String()),
	)
	return h.Handler.Handle(ctx, out)
}

func (h traceHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	return traceHandler{Handler: h.Handler.WithAttrs(attrs)}
}

func (h traceHandler) WithGroup(name string) slog.Handler {
	return traceHandler{Handler: h.Handler.WithGroup(name)}
}
