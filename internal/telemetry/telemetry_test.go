package telemetry

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"testing"
	"time"

	"github.com/go-logr/logr"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
)

// Every test here declares EVERY variable Setup reads, through setTelemetryEnv, and then
// sets what it needs. Reading one from the ambient environment is how a suite passes
// locally and fails in CI, or the reverse.

// setTelemetryEnv clears every variable this package reads, for the rest of the test.
func setTelemetryEnv(t *testing.T) {
	t.Helper()
	for _, key := range []string{
		"OTEL_SDK_DISABLED", "OTEL_SERVICE_NAME", "HOROPTER_TRACE_DISABLE",
		"OTEL_EXPORTER_OTLP_ENDPOINT", "OTEL_EXPORTER_OTLP_TRACES_ENDPOINT",
		"OTEL_EXPORTER_OTLP_METRICS_ENDPOINT",
		"OTEL_EXPORTER_PROMETHEUS_HOST", "OTEL_EXPORTER_PROMETHEUS_PORT",
	} {
		t.Setenv(key, "")
	}
}

// restoreGlobalProviders puts back the tracer and meter providers a test replaces.
//
// OTel's global providers delegate ONE WAY: instruments and tracers handed out before a
// provider was installed stay bound to it. This restores what the getters return, not that
// delegation, which is why every test that installs a provider must also shut it down —
// Flush does, for a Telemetry.
func restoreGlobalProviders(t *testing.T) {
	t.Helper()
	tracers, meters := otel.GetTracerProvider(), otel.GetMeterProvider()
	t.Cleanup(func() {
		otel.SetTracerProvider(tracers)
		otel.SetMeterProvider(meters)
	})
}

// captureSlog routes slog's default logger to a buffer for the rest of the test and returns
// the buffer.
//
// The cleanup reinstalls OTel's logger against the restored default handler, because
// otel.SetLogger captures the handler rather than reading slog.Default() per call.
// Restoring only slog.Default() would leave OTel's logger writing into a dead buffer.
func captureSlog(t *testing.T) *bytes.Buffer {
	t.Helper()
	var logged bytes.Buffer
	previous := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logged, nil)))
	t.Cleanup(func() {
		slog.SetDefault(previous)
		otel.SetLogger(logr.FromSlogHandler(previous.Handler()))
	})
	return &logged
}

func TestConfigFromEnvDisabledWithoutAnEndpoint(t *testing.T) {
	// The OTel default endpoint is localhost:4318. Honouring it would make every
	// developer run without a collector emit a stream of export failures, so an
	// absent endpoint means off rather than "use the default".
	setTelemetryEnv(t)
	cfg := configFromEnv("horopter-ingress")
	assert.False(t, cfg.tracing)
	assert.False(t, cfg.otlpMetrics)
}

// TestConfigFromEnvTracesEndpointEnablesOnlyTracing covers the normal shape when traces
// and metrics go to different collectors. The signal-specific variable is the one the
// exporter itself prefers, so gating on the general one alone gave that operator a daemon
// that started happily and emitted nothing — and it must turn on its own signal only.
func TestConfigFromEnvTracesEndpointEnablesOnlyTracing(t *testing.T) {
	setTelemetryEnv(t)
	t.Setenv("OTEL_EXPORTER_OTLP_TRACES_ENDPOINT", "http://collector:4318/v1/traces")
	cfg := configFromEnv("horopter-ingress")
	assert.True(t, cfg.tracing)
	assert.False(t, cfg.otlpMetrics, "a traces endpoint must not turn on OTLP metrics")
}

func TestConfigFromEnvMetricsEndpointEnablesOnlyOTLPMetrics(t *testing.T) {
	setTelemetryEnv(t)
	t.Setenv("OTEL_EXPORTER_OTLP_METRICS_ENDPOINT", "http://collector:4318/v1/metrics")
	cfg := configFromEnv("horopter-ingress")
	assert.True(t, cfg.otlpMetrics)
	assert.False(t, cfg.tracing, "a metrics endpoint must not turn on tracing")
}

// OTEL_SDK_DISABLED is a hard off for the signal-specific endpoint too, not only for the
// general one — the endpoint variables must share every other rule.
func TestConfigFromEnvSDKDisabledOverridesTheSignalSpecificEndpoint(t *testing.T) {
	setTelemetryEnv(t)
	t.Setenv("OTEL_EXPORTER_OTLP_TRACES_ENDPOINT", "http://collector:4318/v1/traces")
	t.Setenv("OTEL_EXPORTER_OTLP_METRICS_ENDPOINT", "http://collector:4318/v1/metrics")
	t.Setenv("OTEL_SDK_DISABLED", "true")
	cfg := configFromEnv("horopter-ingress")
	assert.False(t, cfg.tracing)
	assert.False(t, cfg.otlpMetrics)
}

func TestConfigFromEnvGeneralEndpointEnablesBothOTLPSignals(t *testing.T) {
	setTelemetryEnv(t)
	t.Setenv("OTEL_EXPORTER_OTLP_ENDPOINT", "http://collector:4318")
	cfg := configFromEnv("horopter-ingress")
	assert.True(t, cfg.tracing)
	assert.True(t, cfg.otlpMetrics)
	assert.Equal(t, "horopter-ingress", cfg.serviceName)
}

func TestConfigFromEnvSDKDisabledOverridesAnEndpoint(t *testing.T) {
	setTelemetryEnv(t)
	t.Setenv("OTEL_EXPORTER_OTLP_ENDPOINT", "http://collector:4318")
	t.Setenv("OTEL_SDK_DISABLED", "true")
	cfg := configFromEnv("horopter-ingress")
	assert.True(t, cfg.sdkDisabled)
	assert.False(t, cfg.tracing, "OTEL_SDK_DISABLED is a hard off regardless of any other variable")
	assert.False(t, cfg.otlpMetrics)
}

func TestConfigFromEnvServiceNameOverride(t *testing.T) {
	setTelemetryEnv(t)
	t.Setenv("OTEL_SERVICE_NAME", "horopter-canary")
	assert.Equal(t, "horopter-canary", configFromEnv("horopter-ingress").serviceName)
}

func TestConfigFromEnvDisabledAreas(t *testing.T) {
	setTelemetryEnv(t)
	t.Setenv("HOROPTER_TRACE_DISABLE", " sql , http ")
	cfg := configFromEnv("horopter-ingress")
	assert.True(t, cfg.disabled["sql"], "whitespace around an area must not defeat it")
	assert.True(t, cfg.disabled["http"])
	assert.False(t, cfg.disabled["store"])
}

// The OpenTelemetry specification's own defaults, kept deliberately (spec §2): an operator
// who knows the specification gets what they expect.
func TestConfigFromEnvPrometheusDefaults(t *testing.T) {
	setTelemetryEnv(t)
	cfg := configFromEnv("horopter-ingress")
	assert.Equal(t, "localhost", cfg.prometheusHost)
	assert.Equal(t, "9464", cfg.prometheusPort)
}

func TestConfigFromEnvPrometheusOverrides(t *testing.T) {
	setTelemetryEnv(t)
	t.Setenv("OTEL_EXPORTER_PROMETHEUS_HOST", "0.0.0.0")
	t.Setenv("OTEL_EXPORTER_PROMETHEUS_PORT", "9470")
	cfg := configFromEnv("horopter-ingress")
	assert.Equal(t, "0.0.0.0", cfg.prometheusHost)
	assert.Equal(t, "9470", cfg.prometheusPort)
}

func TestSetupSucceedsWhenEnabled(t *testing.T) {
	// No collector is listening. The OTLP HTTP exporter connects lazily, so this
	// exercises provider construction — including the resource merge — without one.
	//
	// Setup swallows a tracer provider construction failure by design (see its doc
	// comment), so the assertion that stands in for "no error" is that the global tracer
	// provider was replaced: a schema-URL conflict in newResource returns before
	// otel.SetTracerProvider, and the provider then stays exactly what it was.
	setTelemetryEnv(t)
	t.Setenv("OTEL_EXPORTER_OTLP_ENDPOINT", "http://127.0.0.1:4318")
	restoreGlobalProviders(t)

	prev := otel.GetTracerProvider()
	tel, err := Setup(context.Background(), "horopter-test")
	require.NoError(t, err)
	defer tel.Flush()

	assert.NotSame(t, prev, otel.GetTracerProvider(), "an enabled Setup must install a provider")
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
	setTelemetryEnv(t)
	t.Setenv("OTEL_EXPORTER_OTLP_ENDPOINT", "http://127.0.0.1:4318")
	t.Setenv("HOROPTER_TRACE_DISABLE", "sql")
	restoreGlobalProviders(t)

	tel, err := Setup(context.Background(), "horopter-test")
	require.NoError(t, err)
	defer tel.Flush()

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
}

// TestSetupRoutesSDKErrorsThroughSlog pins the error handler. Without it, an export the
// Collector rejects reaches otel.Handle's default handler and is written to stderr by the
// stdlib logger — the one failure in these binaries not in the same structured stream as
// every other, and the one an operator is least likely to have a filter for.
//
// It drives installTracing() rather than Setup so the assertion does not depend on which
// environment variables happen to enable tracing, and it shuts down the provider install
// leaves behind for the reason TestInstrumentedPathIsInertWithNoProvider documents.
//
// The variables are still declared. installTracing() takes an explicit config, so the two
// switches below are not read on this path — but otlptracehttp.New is, and it builds the
// exporter from the ambient OTEL_EXPORTER_OTLP_* family. Declaring every variable through
// setTelemetryEnv matches the other Setup tests, so the file has one shape, and means this
// test cannot start reading the environment if it is ever switched from installTracing to
// Setup.
func TestSetupRoutesSDKErrorsThroughSlog(t *testing.T) {
	setTelemetryEnv(t)
	t.Setenv("OTEL_EXPORTER_OTLP_ENDPOINT", "http://127.0.0.1:4318")
	logged := captureSlog(t)

	prev := otel.GetTracerProvider()
	shutdown, err := installTracing(context.Background(),
		config{tracing: true, serviceName: "horopter-test", disabled: map[string]bool{}})
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

// TestSetupRoutesSDKConfigErrorsThroughSlog covers the channel the error handler above
// does NOT cover, and the one an operator is actually likely to hit.
//
// An unparsable endpoint is not an error the exporter returns — otlptracehttp.New reports
// it through the SDK's internal logger, ignores it and falls back to its defaults, so the
// log line is the ONLY signal that the collector address was not understood. This drives
// the real mistake rather than a synthesised one, which is also what pins that the logger
// is installed before the exporter is built: install it afterwards and the parse error
// this asserts on has already gone to stderr.
func TestSetupRoutesSDKConfigErrorsThroughSlog(t *testing.T) {
	setTelemetryEnv(t)
	t.Setenv("OTEL_EXPORTER_OTLP_TRACES_ENDPOINT", "://not a url")
	logged := captureSlog(t)

	prev := otel.GetTracerProvider()
	shutdown, err := installTracing(context.Background(),
		config{tracing: true, serviceName: "horopter-test", disabled: map[string]bool{}})
	t.Cleanup(func() { otel.SetTracerProvider(prev) })
	require.NoError(t, err,
		"the SDK ignores an unparsable endpoint rather than failing, which is exactly why "+
			"the log line has to be visible")

	assert.Contains(t, logged.String(), "parse url",
		"a malformed OTEL_EXPORTER_OTLP_* value must reach slog, not the stdlib logger")
	assert.Contains(t, logged.String(), "level=ERROR",
		"the SDK reports a configuration failure at logr's Error level, which is ERROR here")

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	assert.NoError(t, shutdown(ctx))
}

func TestNewResourceMergesWithoutASchemaConflict(t *testing.T) {
	// resource.Merge REJECTS two resources carrying different schema URLs, and the
	// version of the semconv import above is the only thing deciding whether Horopter's
	// explicit service.name agrees with the one resource.Default() carries. An SDK
	// upgrade that moves the schema URL breaks every binary's startup here and nowhere
	// else, so it gets its own assertion. Installs no provider and touches no global.
	_, err := newResource("horopter-test")
	require.NoError(t, err, "semconv must match the version resource.Default() uses")
}

// TestSetupWithSDKDisabledInstallsNothing pins OTEL_SDK_DISABLED as the one switch that
// turns off every signal, including the meter provider behind /metrics, even with a
// collector endpoint configured.
func TestSetupWithSDKDisabledInstallsNothing(t *testing.T) {
	setTelemetryEnv(t)
	t.Setenv("OTEL_SDK_DISABLED", "true")
	t.Setenv("OTEL_EXPORTER_OTLP_ENDPOINT", "http://127.0.0.1:4318")
	restoreGlobalProviders(t)

	tracers, meters := otel.GetTracerProvider(), otel.GetMeterProvider()
	tel, err := Setup(context.Background(), "horopter-test")
	require.NoError(t, err)
	require.NotNil(t, tel, "Setup must never return a nil Telemetry with a nil error")
	defer tel.Flush()

	assert.Same(t, tracers, otel.GetTracerProvider(), "a disabled SDK installs no tracer provider")
	assert.Same(t, meters, otel.GetMeterProvider(), "a disabled SDK installs no meter provider")
}

// TestSetupWithoutAnEndpointInstallsMetricsButNotTracing is the default configuration:
// tracing needs a collector, and metrics do not, because /metrics is on by default.
func TestSetupWithoutAnEndpointInstallsMetricsButNotTracing(t *testing.T) {
	setTelemetryEnv(t)
	restoreGlobalProviders(t)

	tracers, meters := otel.GetTracerProvider(), otel.GetMeterProvider()
	tel, err := Setup(context.Background(), "horopter-test")
	require.NoError(t, err)
	defer tel.Flush()

	assert.Same(t, tracers, otel.GetTracerProvider(), "tracing needs a collector endpoint")
	assert.NotSame(t, meters, otel.GetMeterProvider(), "a meter provider is installed by default")
}

// TestSetupDoesNotFailOnAMalformedOTLPEndpoint pins the half of the failure policy the OTel
// design enforced through Setup's signature, which returned no error. Setup returns one now,
// for the Prometheus reader, so "an OTLP problem never aborts startup" is pinned here, with
// the misconfiguration an operator actually makes. The SDK's ERROR line is captured so it
// does not land in the test output.
func TestSetupDoesNotFailOnAMalformedOTLPEndpoint(t *testing.T) {
	setTelemetryEnv(t)
	t.Setenv("OTEL_EXPORTER_OTLP_ENDPOINT", "://not a url")
	restoreGlobalProviders(t)
	captureSlog(t)

	tel, err := Setup(context.Background(), "horopter-test")
	require.NoError(t, err, "a malformed OTLP endpoint must never stop a daemon starting")
	tel.Flush()
}

// TestFlushIsIdempotent pins that a second Flush does nothing. Shutting a meter provider
// down twice makes its reader report ErrReaderShutdown, which Flush would otherwise log as a
// failed flush for work that had already succeeded.
func TestFlushIsIdempotent(t *testing.T) {
	setTelemetryEnv(t)
	restoreGlobalProviders(t)
	logged := captureSlog(t)

	tel, err := Setup(context.Background(), "horopter-test")
	require.NoError(t, err)
	tel.Flush()
	tel.Flush()

	assert.NotContains(t, logged.String(), "level=WARN",
		"a second Flush must not report the first one's work as a failure")
}

// TestInstrumentedPathIsInertWithNoProvider checks that an instrumented call site is
// safe with no provider installed: it must not panic, allocate a real span, or record.
//
// Be honest about what this asserts under a full-package run: OTel's global tracer
// provider delegates one way, so a provider a sibling test installs is never truly
// removed from the process. This test passes in-suite only because every test in this
// package that installs a tracer provider shuts it down — through Telemetry.Flush, a
// returned shutdown function or a t.Cleanup — and a shut-down SDK provider hands out noop
// tracers. Test files run in name order, so metrics_test.go's tests run before this one.
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
