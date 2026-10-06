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
//
// The SDK keeps the attributes a filter removes on the measurement's exemplar, so the view
// also drops them there. Kept, they put the caller's Host back into the export, and a long
// one pushes the exemplar past Prometheus's 128-rune limit: the exporter then drops it and
// reports an error on every scrape.
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

// withoutFilteredAttributes selects the SDK's default exemplar reservoir for agg, wrapped so
// that it never stores the attributes the view filtered out.
func withoutFilteredAttributes(agg sdkmetric.Aggregation) exemplar.ReservoirProvider {
	provider := sdkmetric.DefaultExemplarReservoirProviderSelector(agg)
	return func(kept attribute.Set) exemplar.Reservoir {
		return filteredAttributesDropped{provider(kept)}
	}
}

// filteredAttributesDropped is an exemplar.Reservoir that offers each measurement without its
// filtered attributes.
type filteredAttributesDropped struct{ exemplar.Reservoir }

func (r filteredAttributesDropped) Offer(
	ctx context.Context, t time.Time, val exemplar.Value, _ []attribute.KeyValue,
) {
	r.Reservoir.Offer(ctx, t, val, nil)
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

// ServeMetrics binds the /metrics listener and serves it in the background.
//
// The bind happens BEFORE it returns, so an invalid port, a host that does not resolve or an
// address already in use is an error the caller sees and exits on. The endpoint is on by
// default, and one that was meant to be there and silently was not would look like a working
// configuration. With OTEL_SDK_DISABLED=true there is no registry, and it does nothing — the
// Prometheus variables are not even validated.
//
// The listener is deliberately not wrapped in otelhttp: a scrape every 15 seconds would
// otherwise dominate the HTTP request metrics and produce a telemetry span per scrape.
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
		// The same bounds the API servers use, for the same reasons: a slow or idle client
		// must not hold a connection, and so a file descriptor, indefinitely.
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

// serveMetrics runs the metrics server until Flush shuts it down. An error after a
// successful bind is logged rather than stopping the daemon: its API is still serving.
func serveMetrics(server *http.Server, listener net.Listener) {
	if err := server.Serve(listener); err != nil && !errors.Is(err, http.ErrServerClosed) {
		slog.Error("the metrics server stopped", "error", err)
	}
}

// metricsAddr validates the port and joins it to the host. The port is re-rendered from the
// parsed integer, so a form Atoi accepts, such as +9464, is normalised before it reaches
// the bind.
func metricsAddr(host, port string) (string, error) {
	n, err := strconv.Atoi(port)
	if err != nil || n < 1 || n > 65535 {
		return "", fmt.Errorf("OTEL_EXPORTER_PROMETHEUS_PORT=%q is not a port; "+
			"expected an integer from 1 to 65535", port)
	}
	return net.JoinHostPort(host, strconv.Itoa(n)), nil
}
