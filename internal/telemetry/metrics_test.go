package telemetry

import (
	"bytes"
	"context"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

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

// TestHTTPMetricsExportWithoutErrorsForAnyHost pins that a sampled request's exemplar
// survives the scrape whatever Host it arrived with. Prometheus refuses an exemplar whose
// labels exceed 128 runes and the exporter reports each refusal through otel.Handle, so a
// long Host — horopter-flux calls ingress by its full service name — logged a warning per scrape.
func TestHTTPMetricsExportWithoutErrorsForAnyHost(t *testing.T) {
	for name, host := range map[string]string{
		"a pod IP, as kubelet's probes send":          "10.244.2.243:8080",
		"a full service name, as horopter-flux sends": "horopter-ingress.horopter.svc.cluster.local:8080",
	} {
		t.Run(name, func(t *testing.T) {
			setTelemetryEnv(t)
			restoreGlobalProviders(t)
			recordTraces(t)
			logged := captureSlog(t)

			tel, err := Setup(context.Background(), "horopter-test")
			require.NoError(t, err)
			defer tel.Flush()

			handler := otelhttp.NewHandler(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(http.StatusOK)
			}), "")
			req := httptest.NewRequest(http.MethodPost, "/v1/events", nil)
			req.Host = host
			handler.ServeHTTP(httptest.NewRecorder(), req)

			exemplars := exemplarLabelNames(t, tel.registry, "http_server_")
			require.Contains(t, exemplars, "trace_id", "the request's exemplar must still be exported")
			assert.NotContains(t, exemplars, "server_address")
			assert.NotContains(t, exemplars, "server_port")
			assert.NotContains(t, logged.String(), "opentelemetry reported an error")
		})
	}
}

// exemplarLabelNames returns every label name on every histogram bucket exemplar whose family
// name starts with prefix.
func exemplarLabelNames(t *testing.T, registry *prometheus.Registry, prefix string) []string {
	t.Helper()
	families, err := registry.Gather()
	require.NoError(t, err)
	var names []string
	for _, family := range families {
		if !strings.HasPrefix(family.GetName(), prefix) {
			continue
		}
		for _, series := range family.GetMetric() {
			for _, bucket := range series.GetHistogram().GetBucket() {
				for _, label := range bucket.GetExemplar().GetLabel() {
					names = append(names, label.GetName())
				}
			}
		}
	}
	return names
}

// freePort returns a port nothing is listening on, by binding :0 and releasing it. Another
// process could take the port in between; that race is preferred to adding anything to
// production code for tests.
func freePort(t *testing.T) string {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	port := strconv.Itoa(listener.Addr().(*net.TCPAddr).Port)
	require.NoError(t, listener.Close())
	return port
}

// fetch sends one request and returns its status and body.
func fetch(t *testing.T, method, url string) (int, string) {
	t.Helper()
	req, err := http.NewRequestWithContext(context.Background(), method, url, nil)
	require.NoError(t, err)
	resp, err := (&http.Client{Timeout: 5 * time.Second}).Do(req)
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	return resp.StatusCode, string(body)
}

// serving starts Setup and ServeMetrics on a free loopback port and returns the base URL.
func serving(t *testing.T) (*Telemetry, string) {
	t.Helper()
	captureSlog(t) // ServeMetrics logs "serving metrics" at INFO; keep it out of -v output.
	port := freePort(t)
	t.Setenv("OTEL_EXPORTER_PROMETHEUS_HOST", "127.0.0.1")
	t.Setenv("OTEL_EXPORTER_PROMETHEUS_PORT", port)
	tel, err := Setup(context.Background(), "horopter-test")
	require.NoError(t, err)
	t.Cleanup(tel.Flush)
	require.NoError(t, tel.ServeMetrics())
	return tel, "http://127.0.0.1:" + port
}

// TestServeMetricsRejectsAnInvalidPort pins that a bad port is an error naming the variable
// and the value, returned to the caller — which exits — rather than a log line.
func TestServeMetricsRejectsAnInvalidPort(t *testing.T) {
	for _, port := range []string{"abc", "0", "65536"} {
		t.Run(port, func(t *testing.T) {
			tel := &Telemetry{
				cfg:      config{prometheusHost: "127.0.0.1", prometheusPort: port},
				registry: prometheus.NewRegistry(),
			}
			err := tel.ServeMetrics()
			require.Error(t, err)
			assert.Contains(t, err.Error(), "OTEL_EXPORTER_PROMETHEUS_PORT")
			assert.Contains(t, err.Error(), strconv.Quote(port))
		})
	}
}

// TestServeMetricsRefusesAnAddressInUse pins the synchronous bind. A listener bound inside
// the serving goroutine would report "address already in use" as a log line some time
// later, and the daemon would run on without the endpoint it was configured to serve.
func TestServeMetricsRefusesAnAddressInUse(t *testing.T) {
	held, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	t.Cleanup(func() { _ = held.Close() })
	port := strconv.Itoa(held.Addr().(*net.TCPAddr).Port)

	tel := &Telemetry{
		cfg:      config{prometheusHost: "127.0.0.1", prometheusPort: port},
		registry: prometheus.NewRegistry(),
	}
	err = tel.ServeMetrics()
	require.Error(t, err, "a bind failure must be returned before ServeMetrics returns")
	assert.Contains(t, err.Error(), port)
}

// TestServeMetricsDoesNothingWithSDKDisabled pins that the Prometheus variables are not
// validated under OTEL_SDK_DISABLED=true: a malformed port must not stop a daemon whose
// metrics are switched off.
func TestServeMetricsDoesNothingWithSDKDisabled(t *testing.T) {
	setTelemetryEnv(t)
	t.Setenv("OTEL_SDK_DISABLED", "true")
	t.Setenv("OTEL_EXPORTER_PROMETHEUS_PORT", "not-a-port")
	restoreGlobalProviders(t)

	tel, err := Setup(context.Background(), "horopter-test")
	require.NoError(t, err)
	defer tel.Flush()
	assert.NoError(t, tel.ServeMetrics())
}

func TestServeMetricsServesRecordedMetrics(t *testing.T) {
	setTelemetryEnv(t)
	restoreGlobalProviders(t)
	_, base := serving(t)

	counter, err := otel.Meter("horopter/test").Int64Counter("horopter.test.scrapes")
	require.NoError(t, err)
	counter.Add(context.Background(), 3)

	status, body := fetch(t, http.MethodGet, base+"/metrics")
	require.Equal(t, http.StatusOK, status)
	assert.Regexp(t, `(?m)^horopter_test_scrapes_total(\{[^}]*\})? 3$`, body,
		"a counter recorded through the global meter provider must be served")
	assert.Regexp(t, `(?m)^go_goroutines \d+$`, body, "the Go runtime collector must be registered")

	status, _ = fetch(t, http.MethodGet, base+"/other")
	assert.Equal(t, http.StatusNotFound, status, "the listener serves /metrics and nothing else")
	status, _ = fetch(t, http.MethodPost, base+"/metrics")
	assert.Equal(t, http.StatusMethodNotAllowed, status)
}

// TestMetricsListenerIsNotInstrumented pins that scrapes are invisible to telemetry. A
// scrape every 15 seconds would otherwise dominate the HTTP request metrics and produce a
// telemetry span per scrape.
func TestMetricsListenerIsNotInstrumented(t *testing.T) {
	setTelemetryEnv(t)
	restoreGlobalProviders(t)
	spans := recordTraces(t)
	tel, base := serving(t)

	for range 2 {
		status, _ := fetch(t, http.MethodGet, base+"/metrics")
		require.Equal(t, http.StatusOK, status)
	}

	assert.Empty(t, spans.Ended(), "a scrape must not produce a telemetry span")
	assert.Empty(t, labelNames(t, tel.registry, "http_server_"),
		"a scrape must not be recorded in the HTTP request metrics")
}

func TestFlushStopsTheMetricsListener(t *testing.T) {
	setTelemetryEnv(t)
	restoreGlobalProviders(t)
	tel, base := serving(t)

	tel.Flush()

	addr := strings.TrimPrefix(base, "http://")
	conn, err := net.DialTimeout("tcp", addr, time.Second)
	if err == nil {
		_ = conn.Close()
	}
	assert.Error(t, err, "Flush must stop the metrics listener")
}

// syncBuffer collects log output written from another goroutine. serveMetrics logs from the
// goroutine ServeMetrics spawns, so a plain bytes.Buffer read by the test would be a data
// race rather than an assertion.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// TestFlushShutsDownQuietly pins that an ordinary shutdown logs nothing at ERROR.
//
// serveMetrics reports any Serve error that is not http.ErrServerClosed, so the order in
// which Flush closes the server and the listener decides whether every shutdown produces an
// error line. Closing the listener first wakes Accept with a plain "use of closed network
// connection" while the server still considers itself running; closing the server first
// makes Serve report ErrServerClosed instead. Nothing else in this package tells the two
// apart, which is how the wrong order reached a commit.
func TestFlushShutsDownQuietly(t *testing.T) {
	setTelemetryEnv(t)
	restoreGlobalProviders(t)

	logged := &syncBuffer{}
	previous := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(logged, nil)))
	t.Cleanup(func() { slog.SetDefault(previous) })

	port := freePort(t)
	t.Setenv("OTEL_EXPORTER_PROMETHEUS_HOST", "127.0.0.1")
	t.Setenv("OTEL_EXPORTER_PROMETHEUS_PORT", port)
	tel, err := Setup(context.Background(), "horopter-test")
	require.NoError(t, err)
	t.Cleanup(tel.Flush)
	require.NoError(t, tel.ServeMetrics())

	// A served scrape proves the goroutine reached Serve and registered the listener with
	// the server, which is where a daemon sits for its whole life before Flush arrives.
	// The wrong close order only produces its error from that state.
	status, _ := fetch(t, http.MethodGet, "http://127.0.0.1:"+port+"/metrics")
	require.Equal(t, http.StatusOK, status)

	tel.Flush()

	// Serve returns on its own goroutine, so an error -- if there is going to be one -- is
	// logged after Flush has returned. Never polls the window instead of reading once.
	assert.Never(t, func() bool {
		return strings.Contains(logged.String(), "level=ERROR")
	}, 250*time.Millisecond, 10*time.Millisecond,
		"an ordinary shutdown must not log an error")
}

// TestFlushDoesNotWaitForAnIdleConnection pins Close over Shutdown for the metrics server.
// A TCP connection to /metrics that never sends a request is exactly the case
// http.Server.Shutdown waits out — it treats a brand new connection as idle only once it
// has sat unread for 5 seconds, which is FlushTimeout itself — so Shutdown here would spend
// the whole flush budget on a connection that scraped nothing, leaving none for the final
// OTLP push or span flush that follow.
func TestFlushDoesNotWaitForAnIdleConnection(t *testing.T) {
	setTelemetryEnv(t)
	restoreGlobalProviders(t)
	tel, base := serving(t)

	conn, err := net.Dial("tcp", strings.TrimPrefix(base, "http://"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = conn.Close() })

	start := time.Now()
	tel.Flush()
	elapsed := time.Since(start)

	assert.Less(t, elapsed, 2*time.Second,
		"Flush must close the metrics listener rather than wait for this idle connection")
}

// otlpRequestRecorder answers every request 200 and records its method and path, so a test
// can assert that a specific request reached the collector without standing up a real OTLP
// server to decode the body against.
type otlpRequestRecorder struct {
	mu       sync.Mutex
	requests []string
}

func (r *otlpRequestRecorder) ServeHTTP(w http.ResponseWriter, req *http.Request) {
	r.mu.Lock()
	r.requests = append(r.requests, req.Method+" "+req.URL.Path)
	r.mu.Unlock()
	w.WriteHeader(http.StatusOK)
}

func (r *otlpRequestRecorder) seen(want string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, got := range r.requests {
		if got == want {
			return true
		}
	}
	return false
}

// TestFlushPushesMetricsOverOTLP pins spec §4's "the meter provider performs the final OTLP
// push" on Flush. Nothing else in this suite drives an OTLP metric export end to end:
// changing installMetrics's `if t.cfg.otlpMetrics {` guard to never run leaves every other
// test green.
func TestFlushPushesMetricsOverOTLP(t *testing.T) {
	recorder := &otlpRequestRecorder{}
	server := httptest.NewServer(recorder)
	defer server.Close()

	setTelemetryEnv(t)
	t.Setenv("OTEL_EXPORTER_OTLP_METRICS_ENDPOINT", server.URL+"/v1/metrics")
	restoreGlobalProviders(t)

	tel, err := Setup(context.Background(), "horopter-test")
	require.NoError(t, err)

	counter, err := otel.Meter("horopter/test").Int64Counter("horopter.test.otlp_export")
	require.NoError(t, err)
	counter.Add(context.Background(), 1)

	tel.Flush()

	assert.True(t, recorder.seen(http.MethodPost+" /v1/metrics"),
		"Flush must push the accumulated metrics to the OTLP collector before shutting down")
}

// TestInstallMetricsRoutesOTLPParseErrorsThroughSlog pins routeDiagnosticsToSlog()'s call
// inside installMetrics: without it, an unparsable
// OTEL_EXPORTER_OTLP_METRICS_ENDPOINT reaches the SDK's internal logger, which writes to
// the stdlib logger on stderr instead of slog, and the suite stays green.
func TestInstallMetricsRoutesOTLPParseErrorsThroughSlog(t *testing.T) {
	setTelemetryEnv(t)
	restoreGlobalProviders(t)
	logged := captureSlog(t)
	t.Setenv("OTEL_EXPORTER_OTLP_METRICS_ENDPOINT", "://not a url")

	tel, err := Setup(context.Background(), "horopter-test")
	require.NoError(t, err, "a malformed OTLP endpoint must never stop a daemon starting")
	tel.Flush()

	assert.Contains(t, logged.String(), "parse url",
		"a malformed OTEL_EXPORTER_OTLP_METRICS_ENDPOINT must reach slog, not the stdlib logger")
	assert.Contains(t, logged.String(), "level=ERROR",
		"the SDK reports a configuration failure at logr's Error level, which is ERROR here")
}
