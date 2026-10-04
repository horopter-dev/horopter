package logging

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"os"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
)

// record logs through a handler over buf and returns the single JSON object it wrote.
// Every test here asserts on emitted JSON, so a change that stops emitting a field fails
// the test that names the field.
func record(t *testing.T, log func(l *slog.Logger)) map[string]any {
	t.Helper()
	var buf bytes.Buffer
	log(slog.New(Handler(&buf, slog.LevelDebug)))
	var out map[string]any
	require.NoError(t, json.Unmarshal(buf.Bytes(), &out), "handler wrote %q", buf.String())
	return out
}

// spanContext returns a context carrying a real recording span, and its ids as strings.
func spanContext(t *testing.T) (context.Context, string, string) {
	t.Helper()
	tp := sdktrace.NewTracerProvider()
	t.Cleanup(func() { _ = tp.Shutdown(context.Background()) })
	ctx, span := tp.Tracer("logging_test").Start(context.Background(), "test")
	t.Cleanup(func() { span.End() })
	sc := span.SpanContext()
	require.True(t, sc.IsValid())
	return ctx, sc.TraceID().String(), sc.SpanID().String()
}

func TestTheLevelDefaultsToInfoWhenTheVariableIsUnset(t *testing.T) {
	t.Setenv(LevelVar, "")
	level, err := levelFromEnv()
	require.NoError(t, err)
	assert.Equal(t, slog.LevelInfo, level)
}

func TestEveryLevelParsesWhateverTheCase(t *testing.T) {
	for raw, want := range map[string]slog.Level{
		"debug": slog.LevelDebug, "DEBUG": slog.LevelDebug,
		"info": slog.LevelInfo, "Info": slog.LevelInfo,
		"warn": slog.LevelWarn, "WARN": slog.LevelWarn,
		"error": slog.LevelError, " error ": slog.LevelError,
	} {
		t.Setenv(LevelVar, raw)
		level, err := levelFromEnv()
		require.NoError(t, err, "raw %q", raw)
		assert.Equal(t, want, level, "raw %q", raw)
	}
}

func TestAnUnknownLevelNamesTheVariableAndTheValue(t *testing.T) {
	t.Setenv(LevelVar, "verbose")
	_, err := levelFromEnv()
	require.Error(t, err)
	assert.Contains(t, err.Error(), LevelVar)
	assert.Contains(t, err.Error(), "verbose")
}

func TestARecordCarriesItsFieldsAsJSON(t *testing.T) {
	out := record(t, func(l *slog.Logger) {
		l.Warn("read shed", "route", "/v1/changes", "waited_ms", 500)
	})
	assert.Equal(t, "read shed", out["msg"])
	assert.Equal(t, "WARN", out["level"])
	assert.Equal(t, "/v1/changes", out["route"])
	assert.Equal(t, float64(500), out["waited_ms"])
	assert.NotEmpty(t, out["time"])
}

func TestALoggedContextCarriesTheSpansIDs(t *testing.T) {
	ctx, traceID, spanID := spanContext(t)
	out := record(t, func(l *slog.Logger) { l.InfoContext(ctx, "read shed") })
	assert.Equal(t, traceID, out["trace_id"])
	assert.Equal(t, spanID, out["span_id"])
}

func TestARecordWithNoSpanCarriesNoIDs(t *testing.T) {
	out := record(t, func(l *slog.Logger) { l.Info("starting") })
	assert.NotContains(t, out, "trace_id")
	assert.NotContains(t, out, "span_id")
}

func TestAttributesAddedBeforehandKeepTheIDs(t *testing.T) {
	ctx, traceID, _ := spanContext(t)
	out := record(t, func(l *slog.Logger) {
		l.With("tenant", "acme").InfoContext(ctx, "read shed")
	})
	assert.Equal(t, "acme", out["tenant"])
	assert.Equal(t, traceID, out["trace_id"],
		"WithAttrs must re-wrap, or correlation is lost for every logger built with it")
}

func TestAGroupKeepsTheIDs(t *testing.T) {
	ctx, traceID, _ := spanContext(t)
	out := record(t, func(l *slog.Logger) {
		l.WithGroup("read").InfoContext(ctx, "read shed", "route", "/v1/changes")
	})
	group, ok := out["read"].(map[string]any)
	require.True(t, ok, "expected a \"read\" group, got %v", out)
	assert.Equal(t, "/v1/changes", group["route"])
	// The ids nest inside the open group: a record's attributes always land in the group
	// the handler is in. No production call site uses WithGroup; this pins the behaviour
	// rather than endorsing it.
	assert.Equal(t, traceID, group["trace_id"],
		"WithGroup must re-wrap, or correlation is lost for every logger built with it")
}

func TestSetupInstallsTheConfiguredLevel(t *testing.T) {
	previous := slog.Default()
	t.Cleanup(func() { slog.SetDefault(previous) })
	t.Setenv(LevelVar, "error")

	require.NoError(t, Setup())
	ctx := context.Background()
	assert.False(t, slog.Default().Enabled(ctx, slog.LevelWarn))
	assert.True(t, slog.Default().Enabled(ctx, slog.LevelError))
}

func TestSetupReturnsTheLevelError(t *testing.T) {
	previous := slog.Default()
	t.Cleanup(func() { slog.SetDefault(previous) })
	t.Setenv(LevelVar, "loud")

	require.Error(t, Setup())
	assert.Same(t, previous, slog.Default(), "a failed Setup must not replace the default")
}

// TestSetupInstallsTheTraceCorrelatingHandler proves Setup wires the trace-correlating
// wrapper, not a bare slog.NewJSONHandler. A mutation that swaps in the bare handler leaves
// every other test in this file, and go vet, silent: this is the only test that would catch
// it, so it stands in for Setup's only production path.
func TestSetupInstallsTheTraceCorrelatingHandler(t *testing.T) {
	previousStderr := os.Stderr
	r, w, err := os.Pipe()
	require.NoError(t, err)
	t.Cleanup(func() { _ = r.Close() })
	os.Stderr = w
	t.Cleanup(func() { os.Stderr = previousStderr })

	previous := slog.Default()
	t.Cleanup(func() { slog.SetDefault(previous) })
	t.Setenv(LevelVar, "info")

	require.NoError(t, Setup())

	ctx, traceID, spanID := spanContext(t)
	slog.Default().InfoContext(ctx, "trace correlated")

	require.NoError(t, w.Close())
	data, err := io.ReadAll(r)
	require.NoError(t, err)

	var out map[string]any
	require.NoError(t, json.Unmarshal(data, &out), "Setup's default logger wrote %q", data)
	assert.Equal(t, traceID, out["trace_id"])
	assert.Equal(t, spanID, out["span_id"])
}
