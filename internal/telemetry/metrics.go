package telemetry

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"
	"go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/exporters/otlp/otlpmetric/otlpmetrichttp"
	otelprom "go.opentelemetry.io/otel/exporters/prometheus"
	"go.opentelemetry.io/otel/sdk/instrumentation"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	semconv "go.opentelemetry.io/otel/semconv/v1.43.0"
)

// installMetrics builds the meter provider and installs it globally.
//
// The Prometheus reader is always present, on a dedicated registry rather than
// prometheus.DefaultRegisterer, so there is no process-wide state and a test can inspect
// exactly what this provider exports. The OTLP reader is added only when a collector is
// configured, and a failure to build it is logged and skipped, as a tracer provider failure
// is. Both readers read every instrument: sdkmetric.WithReader appends.
func (t *Telemetry) installMetrics(ctx context.Context) error {
	routeDiagnosticsToSlog()
	res, err := newResource(t.cfg.serviceName)
	if err != nil {
		return err
	}
	registry, reader, err := newPrometheusReader()
	if err != nil {
		return err
	}
	opts := []sdkmetric.Option{
		sdkmetric.WithResource(res),
		sdkmetric.WithReader(reader),
		sdkmetric.WithView(withoutRequestHost()),
	}
	if t.cfg.otlpMetrics {
		opts = append(opts, otlpMetricReader(ctx, t.cfg.serviceName)...)
	}
	t.registry = registry
	t.meterProvider = sdkmetric.NewMeterProvider(opts...)
	otel.SetMeterProvider(t.meterProvider)
	return nil
}

// newPrometheusReader builds the registry /metrics serves and the reader that fills it.
//
// The Go runtime and process collectors are registered beside the exporter: goroutines,
// heap, garbage collection, resident memory and open file descriptors describe the process
// rather than any request, which is what time series are for. Resident memory is the signal
// that would have shown the 2.7 GiB a single unauthenticated read was measured to cost.
func newPrometheusReader() (*prometheus.Registry, sdkmetric.Reader, error) {
	registry := prometheus.NewRegistry()
	for _, collector := range []prometheus.Collector{
		collectors.NewGoCollector(),
		collectors.NewProcessCollector(collectors.ProcessCollectorOpts{}),
	} {
		if err := registry.Register(collector); err != nil {
			return nil, nil, fmt.Errorf("register the %T collector: %w", collector, err)
		}
	}
	reader, err := otelprom.New(otelprom.WithRegisterer(registry))
	if err != nil {
		return nil, nil, fmt.Errorf("create the Prometheus metric reader: %w", err)
	}
	return registry, reader, nil
}

// withoutRequestHost drops server.address and server.port from otelhttp's metrics. With no
// server name configured, otelhttp reads both from the request's Host header, so any caller
// — unauthenticated on egress — could otherwise create a new series per request. Telemetry
// spans keep both attributes: otelhttp.WithServerName would have bounded the metrics too,
// but it overwrites the attributes on spans as well.
func withoutRequestHost() sdkmetric.View {
	return sdkmetric.NewView(
		sdkmetric.Instrument{Name: "*", Scope: instrumentation.Scope{Name: otelhttp.ScopeName}},
		sdkmetric.Stream{AttributeFilter: attribute.NewDenyKeysFilter(
			semconv.ServerAddressKey, semconv.ServerPortKey)},
	)
}

// otlpMetricReader returns the OTLP periodic reader as a provider option, or no option when
// the exporter cannot be built. The interval, timeout and temporality come from the standard
// OTEL_METRIC_EXPORT_* and OTEL_EXPORTER_OTLP_METRICS_TEMPORALITY_PREFERENCE variables, which
// the SDK and exporter read themselves.
func otlpMetricReader(ctx context.Context, serviceName string) []sdkmetric.Option {
	exporter, err := otlpmetrichttp.New(ctx)
	if err != nil {
		slog.Error("OTLP metric export setup failed; continuing without it",
			"service", serviceName, "error", fmt.Errorf("%w; check "+
				"OTEL_EXPORTER_OTLP_ENDPOINT and OTEL_EXPORTER_OTLP_METRICS_ENDPOINT", err))
		return nil
	}
	return []sdkmetric.Option{sdkmetric.WithReader(sdkmetric.NewPeriodicReader(exporter))}
}
