package telemetry

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
)

func TestConfigFromEnvDisabledWithoutAnEndpoint(t *testing.T) {
	// The OTel default endpoint is localhost:4318. Honouring it would make every
	// developer run without a collector emit a stream of export failures, so an
	// absent endpoint means off rather than "use the default".
	t.Setenv("OTEL_EXPORTER_OTLP_ENDPOINT", "")
	t.Setenv("HOROPTER_TRACE_DISABLE", "")
	assert.False(t, configFromEnv("horopter-ingress").enabled)
}

// Every test here sets BOTH variables it depends on. Reading one from the ambient
// environment is how a suite passes locally and fails in CI, or vice versa.
func TestConfigFromEnvEnabledWithAnEndpoint(t *testing.T) {
	t.Setenv("OTEL_EXPORTER_OTLP_ENDPOINT", "http://collector:4318")
	t.Setenv("OTEL_SDK_DISABLED", "")
	cfg := configFromEnv("horopter-ingress")
	assert.True(t, cfg.enabled)
	assert.Equal(t, "horopter-ingress", cfg.serviceName)
}

func TestConfigFromEnvSDKDisabledOverridesAnEndpoint(t *testing.T) {
	t.Setenv("OTEL_EXPORTER_OTLP_ENDPOINT", "http://collector:4318")
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
	t.Setenv("OTEL_EXPORTER_OTLP_ENDPOINT", "http://127.0.0.1:4318")

	prev := otel.GetTracerProvider()
	shutdown, err := Setup(context.Background(), "horopter-test")
	t.Cleanup(func() { otel.SetTracerProvider(prev) })

	require.NoError(t, err, "a schema-URL conflict in newResource surfaces here")
	assert.NotSame(t, prev, otel.GetTracerProvider(), "an enabled Setup must install a provider")

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

	before := otel.GetTracerProvider()
	shutdown, err := Setup(context.Background(), "horopter-test")
	require.NoError(t, err)
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
// removed from the process. This test passes in-suite only because
// TestSetupSucceedsWhenEnabled calls its shutdown function, and a shut-down SDK
// provider hands out noop tracers. Run it in isolation
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
