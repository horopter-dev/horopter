package telemetry

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"strconv"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/exporters/otlp/otlpmetric/otlpmetrichttp"
	otelprom "go.opentelemetry.io/otel/exporters/prometheus"
	"go.opentelemetry.io/otel/sdk/instrumentation"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/exemplar"
	semconv "go.opentelemetry.io/otel/semconv/v1.43.0"
)

// installMetrics builds the meter provider and installs it globally.
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

// withoutRequestHost drops the caller-chosen Host from otelhttp's metrics and exemplars.
func withoutRequestHost() sdkmetric.View {
	return sdkmetric.NewView(
		sdkmetric.Instrument{Name: "*", Scope: instrumentation.Scope{Name: otelhttp.ScopeName}},
		sdkmetric.Stream{
			AttributeFilter: attribute.NewDenyKeysFilter(
				semconv.ServerAddressKey, semconv.ServerPortKey),
			ExemplarReservoirProviderSelector: withoutFilteredAttributes,
		},
	)
}

// withoutFilteredAttributes wraps the default exemplar reservoir to drop filtered attributes.
func withoutFilteredAttributes(agg sdkmetric.Aggregation) exemplar.ReservoirProvider {
	provider := sdkmetric.DefaultExemplarReservoirProviderSelector(agg)
	return func(kept attribute.Set) exemplar.Reservoir {
		return filteredAttributesDropped{provider(kept)}
	}
}

// filteredAttributesDropped offers each measurement without its filtered attributes.
type filteredAttributesDropped struct{ exemplar.Reservoir }

func (r filteredAttributesDropped) Offer(
	ctx context.Context, t time.Time, val exemplar.Value, _ []attribute.KeyValue,
) {
	r.Reservoir.Offer(ctx, t, val, nil)
}

// otlpMetricReader returns the OTLP periodic reader, or nothing when it cannot be built.
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

// ServeMetrics binds the /metrics listener before returning and serves it in the background.
func (t *Telemetry) ServeMetrics() error {
	if t.registry == nil {
		return nil
	}
	addr, err := metricsAddr(t.cfg.prometheusHost, t.cfg.prometheusPort)
	if err != nil {
		return err
	}
	listener, err := net.Listen("tcp", addr)
	if err != nil {
		return fmt.Errorf("listen for /metrics on %s (OTEL_EXPORTER_PROMETHEUS_HOST=%q, "+
			"OTEL_EXPORTER_PROMETHEUS_PORT=%q): %w",
			addr, t.cfg.prometheusHost, t.cfg.prometheusPort, err)
	}
	mux := http.NewServeMux()
	mux.Handle("GET /metrics", promhttp.HandlerFor(t.registry, promhttp.HandlerOpts{}))
	t.metricsServer = &http.Server{
		Handler: mux,
		// The API servers' bounds: a slow client must not hold a file descriptor forever.
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       15 * time.Second,
		WriteTimeout:      30 * time.Second,
		IdleTimeout:       120 * time.Second,
	}
	t.metricsListener = listener
	slog.Info("serving metrics", "addr", listener.Addr().String())
	go serveMetrics(t.metricsServer, listener)
	return nil
}

// serveMetrics runs the metrics server until Flush shuts it down, logging rather than exiting.
func serveMetrics(server *http.Server, listener net.Listener) {
	if err := server.Serve(listener); err != nil && !errors.Is(err, http.ErrServerClosed) {
		slog.Error("the metrics server stopped", "error", err)
	}
}

// metricsAddr validates the port and joins it to the host, normalising forms such as +9464.
func metricsAddr(host, port string) (string, error) {
	n, err := strconv.Atoi(port)
	if err != nil || n < 1 || n > 65535 {
		return "", fmt.Errorf("OTEL_EXPORTER_PROMETHEUS_PORT=%q is not a port; "+
			"expected an integer from 1 to 65535", port)
	}
	return net.JoinHostPort(host, strconv.Itoa(n)), nil
}
