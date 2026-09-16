package telemetry

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
)

// Every test here sets EVERY variable it depends on, including the ones it needs to be
// empty. Reading one from the ambient environment is how a suite passes locally and fails
// in CI, or vice versa.
func TestConfigFromEnvDisabledWithoutAnEndpoint(t *testing.T) {
	// The OTel default endpoint is localhost:4318. Honouring it would make every
	// developer run without a collector emit a stream of export failures, so an
	// absent endpoint means off rather than "use the default".
	t.Setenv("OTEL_EXPORTER_OTLP_ENDPOINT", "")
	t.Setenv("OTEL_EXPORTER_OTLP_TRACES_ENDPOINT", "")
	t.Setenv("HOROPTER_TRACE_DISABLE", "")
	assert.False(t, configFromEnv("horopter-ingress").enabled)
}

// TestConfigFromEnvEnabledWithOnlyTheSignalSpecificEndpoint covers the normal shape when
// traces and metrics go to different collectors: only OTEL_EXPORTER_OTLP_TRACES_ENDPOINT
// is set. It is the variable the exporter itself prefers, so gating on the general one
// alone gave that operator a daemon that started happily and emitted nothing, with
// neither an error nor a log line.
func TestConfigFromEnvEnabledWithOnlyTheSignalSpecificEndpoint(t *testing.T) {
	t.Setenv("OTEL_EXPORTER_OTLP_ENDPOINT", "")
	t.Setenv("OTEL_EXPORTER_OTLP_TRACES_ENDPOINT", "http://collector:4318/v1/traces")
	t.Setenv("OTEL_SDK_DISABLED", "")
	assert.True(t, configFromEnv("horopter-ingress").enabled)
}

// OTEL_SDK_DISABLED is a hard off for the signal-specific endpoint too, not only for the
// general one — the two endpoint variables must share every other rule.
func TestConfigFromEnvSDKDisabledOverridesTheSignalSpecificEndpoint(t *testing.T) {
	t.Setenv("OTEL_EXPORTER_OTLP_ENDPOINT", "")
	t.Setenv("OTEL_EXPORTER_OTLP_TRACES_ENDPOINT", "http://collector:4318/v1/traces")
	t.Setenv("OTEL_SDK_DISABLED", "true")
	assert.False(t, configFromEnv("horopter-ingress").enabled)
}

func TestConfigFromEnvEnabledWithAnEndpoint(t *testing.T) {
	t.Setenv("OTEL_EXPORTER_OTLP_ENDPOINT", "http://collector:4318")
	t.Setenv("OTEL_EXPORTER_OTLP_TRACES_ENDPOINT", "")
	t.Setenv("OTEL_SDK_DISABLED", "")
	t.Setenv("OTEL_SERVICE_NAME", "")
	cfg := configFromEnv("horopter-ingress")
	assert.True(t, cfg.enabled)
	assert.Equal(t, "horopter-ingress", cfg.serviceName)
}

func TestConfigFromEnvSDKDisabledOverridesAnEndpoint(t *testing.T) {
	t.Setenv("OTEL_EXPORTER_OTLP_ENDPOINT", "http://collector:4318")
	t.Setenv("OTEL_EXPORTER_OTLP_TRACES_ENDPOINT", "")
	t.Setenv("OTEL_SDK_DISABLED", "true")
	assert.False(t, configFromEnv("horopter-ingress").enabled,
		"OTEL_SDK_DISABLED is a hard off regardless of any other variable")
}

func TestConfigFromEnvServiceNameOverride(t *testing.T) {
	t.Setenv("OTEL_EXPORTER_OTLP_ENDPOINT", "http://collector:4318")
	t.Setenv("OTEL_SERVICE_NAME", "horopter-canary")
	assert.Equal(t, "horopter-canary", configFromEnv("horopter-ingress").serviceName)
}

func TestConfigFromEnvDisabledAreas(t *testing.T) {
	t.Setenv("OTEL_EXPORTER_OTLP_ENDPOINT", "http://collector:4318")
	t.Setenv("OTEL_SDK_DISABLED", "")
	t.Setenv("HOROPTER_TRACE_DISABLE", " sql , http ")
	cfg := configFromEnv("horopter-ingress")
	assert.True(t, cfg.disabled["sql"], "whitespace around an area must not defeat it")
	assert.True(t, cfg.disabled["http"])
	assert.False(t, cfg.disabled["store"])
}

func TestSetupSucceedsWhenEnabled(t *testing.T) {
	// No collector is listening. The OTLP HTTP exporter connects lazily, so this
	// exercises provider construction — including the resource merge — without one.
	//
	// Setup swallows a construction failure by design (see its doc comment), so the
	// assertion that stands in for "no error" is that the global provider was replaced:
	// a schema-URL conflict in newResource returns before otel.SetTracerProvider, and
	// the provider then stays exactly what it was.
	t.Setenv("OTEL_EXPORTER_OTLP_ENDPOINT", "http://127.0.0.1:4318")
	t.Setenv("OTEL_EXPORTER_OTLP_TRACES_ENDPOINT", "")
	t.Setenv("OTEL_SDK_DISABLED", "")
	t.Setenv("HOROPTER_TRACE_DISABLE", "")

	prev := otel.GetTracerProvider()
	shutdown := Setup(context.Background(), "horopter-test")
	t.Cleanup(func() { otel.SetTracerProvider(prev) })

	assert.NotSame(t, prev, otel.GetTracerProvider(), "an enabled Setup must install a provider")

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	assert.NoError(t, shutdown(ctx))
}

// TestSetupHonoursTheDisabledAreasEndToEnd joins the two halves nothing else joins:
// configFromEnv's parsing of HOROPTER_TRACE_DISABLE and filtered()'s suppression. Each
// is covered in isolation, so Setup could stop passing cfg.disabled to filtered — this
// branch's one novel, non-standard feature — with every other test still green.
//
// It installs a provider, so it runs before TestInstrumentedPathIsInertWithNoProvider
// and shuts down what it installs: OTel's global provider delegates ONE WAY, and a
// shut-down SDK provider handing out noop tracers is what keeps that later test honest.
func TestSetupHonoursTheDisabledAreasEndToEnd(t *testing.T) {
	t.Setenv("OTEL_EXPORTER_OTLP_ENDPOINT", "http://127.0.0.1:4318")
	t.Setenv("OTEL_EXPORTER_OTLP_TRACES_ENDPOINT", "")
	t.Setenv("OTEL_SDK_DISABLED", "")
	t.Setenv("HOROPTER_TRACE_DISABLE", "sql")

	prev := otel.GetTracerProvider()
	shutdown := Setup(context.Background(), "horopter-test")
	t.Cleanup(func() { otel.SetTracerProvider(prev) })

	// IsRecording is read BEFORE End: the SDK's recording span reports false once it has
	// ended, so asserting after End would make the suppressed half pass against a live
	// span and the surviving half impossible to satisfy.
	provider := otel.GetTracerProvider()
	_, sqlSpan := provider.Tracer("github.com/exaring/otelpgx").
		Start(context.Background(), "SELECT")
	sqlRecording := sqlSpan.IsRecording()
	sqlSpan.End()

	_, storeSpan := provider.Tracer(StoreScope).Start(context.Background(), "AppendEvent")
	storeRecording := storeSpan.IsRecording()
	storeSpan.End()

	assert.False(t, sqlRecording,
		"HOROPTER_TRACE_DISABLE=sql must reach filtered() and silence otelpgx's scope")
	assert.True(t, storeRecording,
		"disabling sql must not disable store; a filter that matched everything would "+
			"pass the assertion above on its own")

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	assert.NoError(t, shutdown(ctx))
}

// TestSetupRoutesSDKErrorsThroughSlog pins the error handler. Without it, an export the
// Collector rejects reaches otel.Handle's default handler and is written to stderr by the
// stdlib logger — the one failure in these binaries not in the same structured stream as
// every other, and the one an operator is least likely to have a filter for.
//
// It drives install() rather than Setup so the assertion does not depend on which
// environment variables happen to enable tracing, and it shuts down the provider install
// leaves behind for the reason TestInstrumentedPathIsInertWithNoProvider documents.
func TestSetupRoutesSDKErrorsThroughSlog(t *testing.T) {
	var logged bytes.Buffer
	previous := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logged, nil)))
	t.Cleanup(func() { slog.SetDefault(previous) })

	prev := otel.GetTracerProvider()
	shutdown, err := install(context.Background(),
		config{enabled: true, serviceName: "horopter-test", disabled: map[string]bool{}})
	t.Cleanup(func() { otel.SetTracerProvider(prev) })
	require.NoError(t, err)

	otel.Handle(errors.New("a collector rejected this batch"))
	assert.Contains(t, logged.String(), "a collector rejected this batch",
		"an SDK error must reach slog, not the stdlib logger on stderr")
	assert.Contains(t, logged.String(), "level=WARN")

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	assert.NoError(t, shutdown(ctx))
}

func TestNewResourceMergesWithoutASchemaConflict(t *testing.T) {
	// The B2 defect in one assertion: resource.Merge rejects two resources carrying
	// different schema URLs, and the semconv import version is the only thing that
	// decides. Installs no provider and touches no global.
	_, err := newResource("horopter-test")
	require.NoError(t, err, "semconv must match the version resource.Default() uses")
}

func TestSetupInstallsNothingWhenDisabled(t *testing.T) {
	t.Setenv("OTEL_EXPORTER_OTLP_ENDPOINT", "")
	t.Setenv("OTEL_EXPORTER_OTLP_TRACES_ENDPOINT", "")

	before := otel.GetTracerProvider()
	shutdown := Setup(context.Background(), "horopter-test")
	require.NotNil(t, shutdown, "shutdown must never be nil, so callers need no nil check")

	assert.Same(t, before, otel.GetTracerProvider(),
		"a disabled Setup must not replace the global provider")
	assert.NoError(t, shutdown(context.Background()))
}

// TestInstrumentedPathIsInertWithNoProvider checks that an instrumented call site is
// safe with no provider installed: it must not panic, allocate a real span, or record.
//
// Be honest about what this asserts under a full-package run: OTel's global tracer
// provider delegates one way, so a provider a sibling test installs is never truly
// removed from the process. This test passes in-suite only because every sibling that
// installs a provider — TestSetupSucceedsWhenEnabled,
// TestSetupHonoursTheDisabledAreasEndToEnd and TestSetupRoutesSDKErrorsThroughSlog —
// calls its shutdown function, and a shut-down SDK provider hands out noop tracers.
//
// Run it in isolation
// (go test -run TestInstrumentedPathIsInertWithNoProvider ./internal/telemetry/) to
// assert what its name actually claims; in a full-package run it is really asserting
// "the provider the previous test installed has been shut down."
func TestInstrumentedPathIsInertWithNoProvider(t *testing.T) {
	// No provider installed. This is what every instrumented function in the codebase
	// does on the default path, so if it panicked or allocated a real span the whole
	// no-conditionals design would be wrong.
	_, span := otel.Tracer("horopter/test").Start(context.Background(), "op")
	span.SetAttributes(attribute.String("horopter.tenant", "acme"))
	span.End()

	assert.False(t, span.IsRecording(), "an uninstalled SDK must yield a non-recording span")
	assert.False(t, span.SpanContext().IsValid())
}
