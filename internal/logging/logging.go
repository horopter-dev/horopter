// Package logging installs the process-wide JSON slog handler, adding the active span's ids.
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

// Setup installs the JSON handler as slog's default; call it before telemetry.Setup.
func Setup() error {
	level, err := levelFromEnv()
	if err != nil {
		return err
	}
	slog.SetDefault(slog.New(Handler(os.Stderr, level)))
	return nil
}

// Handler is the handler Setup installs, exported so other tests can log as the daemons do.
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

// traceHandler adds span ids; WithAttrs and WithGroup re-wrap, or the ids silently go.
type traceHandler struct{ slog.Handler }

func (h traceHandler) Handle(ctx context.Context, r slog.Record) error {
	sc := trace.SpanContextFromContext(ctx)
	if !sc.IsValid() {
		return h.Handler.Handle(ctx, r)
	}
	// A Record's attributes may share a backing array with the caller's.
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
