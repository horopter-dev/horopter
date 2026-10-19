package telemetry

import (
	"bytes"
	"context"
	"errors"
	"log"
	"log/slog"
	"testing"
	"time"

	"github.com/go-logr/logr"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
)

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

// restoreGlobalProviders skips an unchanged provider: OTel logs resetting one at ERROR.
func restoreGlobalProviders(t *testing.T) {
	t.Helper()
	tracers, meters := otel.GetTracerProvider(), otel.GetMeterProvider()
	t.Cleanup(func() {
		if current := otel.GetTracerProvider(); current != tracers {
			otel.SetTracerProvider(tracers)
		}
		if current := otel.GetMeterProvider(); current != meters {
			otel.SetMeterProvider(meters)
		}
	})
}

// captureSlog restores the stdlib log package too: slog.SetDefault also redirects it.
func captureSlog(t *testing.T) *bytes.Buffer {
	t.Helper()
	var logged bytes.Buffer
	previousWriter := log.Writer()
	previousFlags := log.Flags()
	previous := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logged, nil)))
	t.Cleanup(func() {
		slog.SetDefault(previous)
		log.SetOutput(previousWriter)
		log.SetFlags(previousFlags)
		otel.SetLogger(logr.FromSlogHandler(previous.Handler()))
	})
	return &logged
}

// TestCaptureSlogRestoresTheStdlibLogPackage uses a subtest so its cleanup runs first.
func TestCaptureSlogRestoresTheStdlibLogPackage(t *testing.T) {
	wantWriter := log.Writer()
	wantFlags := log.Flags()

	t.Run("subtest", func(t *testing.T) {
		captureSlog(t)
	})

	assert.Equal(t, wantWriter, log.Writer(),
		"captureSlog must restore the stdlib log package's writer, not only slog.Default()")
	assert.Equal(t, wantFlags, log.Flags(),
		"captureSlog must restore the stdlib log package's flags, not only slog.Default()")
}

func TestConfigFromEnvDisabledWithoutAnEndpoint(t *testing.T) {
	setTelemetryEnv(t)
	cfg := configFromEnv("horopter-ingress")
	assert.False(t, cfg.tracing)
	assert.False(t, cfg.otlpMetrics)
}

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
	// Setup only logs a tracer provider failure, so a replaced provider stands in for no error.
	setTelemetryEnv(t)
	t.Setenv("OTEL_EXPORTER_OTLP_TRACES_ENDPOINT", "http://127.0.0.1:4318")
	restoreGlobalProviders(t)

	prev := otel.GetTracerProvider()
	tel, err := Setup(context.Background(), "horopter-test")
	require.NoError(t, err)
	defer tel.Flush()

	assert.NotSame(t, prev, otel.GetTracerProvider(), "an enabled Setup must install a provider")
}

// TestSetupHonoursTheDisabledAreasEndToEnd is the only test joining configFromEnv to filtered.
func TestSetupHonoursTheDisabledAreasEndToEnd(t *testing.T) {
	setTelemetryEnv(t)
	t.Setenv("OTEL_EXPORTER_OTLP_TRACES_ENDPOINT", "http://127.0.0.1:4318")
	t.Setenv("HOROPTER_TRACE_DISABLE", "sql")
	restoreGlobalProviders(t)

	tel, err := Setup(context.Background(), "horopter-test")
	require.NoError(t, err)
	defer tel.Flush()

	// IsRecording is read before End: an ended SDK span reports false.
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

// TestSetupRoutesSDKErrorsThroughSlog sets the env because otlptracehttp.New reads it.
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

// TestSetupRoutesSDKConfigErrorsThroughSlog also pins the logger being set before the exporter.
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
	// resource.Merge rejects differing schema URLs, so an SDK upgrade can break startup here.
	_, err := newResource("horopter-test")
	require.NoError(t, err, "semconv must match the version resource.Default() uses")
}

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

func TestSetupDoesNotFailOnAMalformedOTLPEndpoint(t *testing.T) {
	setTelemetryEnv(t)
	t.Setenv("OTEL_EXPORTER_OTLP_ENDPOINT", "://not a url")
	restoreGlobalProviders(t)
	captureSlog(t)

	tel, err := Setup(context.Background(), "horopter-test")
	require.NoError(t, err, "a malformed OTLP endpoint must never stop a daemon starting")
	tel.Flush()
}

// TestFlushIsIdempotent: a second meter provider shutdown reports ErrReaderShutdown.
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

// TestInstrumentedPathIsInertWithNoProvider relies, in-suite, on every test shutting down.
func TestInstrumentedPathIsInertWithNoProvider(t *testing.T) {
	_, span := otel.Tracer("horopter/test").Start(context.Background(), "op")
	span.SetAttributes(attribute.String("horopter.tenant", "acme"))
	span.End()

	assert.False(t, span.IsRecording(), "an uninstalled SDK must yield a non-recording span")
	assert.False(t, span.SpanContext().IsValid())
}
