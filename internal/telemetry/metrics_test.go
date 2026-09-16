package telemetry

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp"
	"go.opentelemetry.io/otel"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
)

// recordTraces installs a recording tracer provider for the rest of the test and shuts it
// down afterwards, so it hands out noop tracers to anything still holding one.
func recordTraces(t *testing.T) *tracetest.SpanRecorder {
	t.Helper()
	spans := tracetest.NewSpanRecorder()
	provider := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(spans))
	otel.SetTracerProvider(provider)
	t.Cleanup(func() { _ = provider.Shutdown(context.Background()) })
	return spans
}

// labelNames returns every label name on every series whose family name starts with prefix.
func labelNames(t *testing.T, registry *prometheus.Registry, prefix string) []string {
	t.Helper()
	families, err := registry.Gather()
	require.NoError(t, err)
	var names []string
	for _, family := range families {
		if !strings.HasPrefix(family.GetName(), prefix) {
			continue
		}
		for _, series := range family.GetMetric() {
			for _, label := range series.GetLabel() {
				names = append(names, label.GetName())
			}
		}
	}
	return names
}

// TestHTTPMetricsDropTheRequestHost pins the one view on the meter provider. With no server
// name configured, otelhttp reads server.address and server.port from the request's Host
// header, so without the view any caller — unauthenticated on egress — could create a new
// series per request. Telemetry spans must keep both: that is why a view was chosen over
// otelhttp.WithServerName, which overwrites them on spans too.
func TestHTTPMetricsDropTheRequestHost(t *testing.T) {
	setTelemetryEnv(t)
	restoreGlobalProviders(t)
	spans := recordTraces(t)

	tel, err := Setup(context.Background(), "horopter-test")
	require.NoError(t, err)
	defer tel.Flush()

	handler := otelhttp.NewHandler(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}), "")
	req := httptest.NewRequest(http.MethodGet, "/v1/changes", nil)
	req.Host = "caller-chosen.example:4242"
	handler.ServeHTTP(httptest.NewRecorder(), req)

	names := labelNames(t, tel.registry, "http_server_")
	require.NotEmpty(t, names,
		"otelhttp recorded no http_server_* series, so the assertions below prove nothing")
	assert.NotContains(t, names, "server_address")
	assert.NotContains(t, names, "server_port")

	ended := spans.Ended()
	require.Len(t, ended, 1)
	var address string
	for _, kv := range ended[0].Attributes() {
		if kv.Key == "server.address" {
			address = kv.Value.AsString()
		}
	}
	assert.Equal(t, "caller-chosen.example", address, "the telemetry span must keep the host")
}
