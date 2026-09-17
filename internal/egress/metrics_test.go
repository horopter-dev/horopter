package egress_test

import (
	"context"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/yo61/horopter/internal/egress"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
)

// TestMain installs a real, readerless MeterProvider before any test runs. The
// go.opentelemetry.io/otel global package delegates to whichever provider is installed FIRST via
// otel.SetMeterProvider, replaying every instrument created against the no-op default onto it —
// and every test in this package that builds a Server (every newServerWith call) creates one via
// egress.New's otel.Meter(meterScope) call. Without this, recordMetrics's own SetMeterProvider,
// the first real one the test binary installs, would be the one instruments from every EARLIER
// test's Server delegate to — so an earlier test's read concurrency limiter, still holding a live
// observable-gauge callback, would keep reporting into a LATER test's ManualReader. Installing a
// real (if inert) provider first, before any Server exists, makes that one-time replay a no-op,
// so recordMetrics's own SetMeterProvider calls are plain swaps that only this package's own
// tests observe.
func TestMain(m *testing.M) {
	otel.SetMeterProvider(sdkmetric.NewMeterProvider())
	os.Exit(m.Run())
}

// recordMetrics installs a meter provider with a manual reader for the rest of the test. Call it
// before egress.New, which creates its instruments from the global provider as it stands.
func recordMetrics(t *testing.T) *sdkmetric.ManualReader {
	t.Helper()
	reader := sdkmetric.NewManualReader()
	provider := sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader))
	prev := otel.GetMeterProvider()
	otel.SetMeterProvider(provider)
	t.Cleanup(func() {
		otel.SetMeterProvider(prev)
		_ = provider.Shutdown(context.Background())
	})
	return reader
}

// readMetricPoints collects every horopter.read.* data point, keyed by the instrument's name
// and its attribute set's encoding joined by "|", such as
// "horopter.read.shed|horopter.read.shed.gate=memory".
func readMetricPoints(t *testing.T, reader *sdkmetric.ManualReader) map[string]int64 {
	t.Helper()
	var rm metricdata.ResourceMetrics
	require.NoError(t, reader.Collect(context.Background(), &rm))
	points := map[string]int64{}
	for _, scope := range rm.ScopeMetrics {
		for _, m := range scope.Metrics {
			if strings.HasPrefix(m.Name, "horopter.read.") {
				addPoints(t, points, m)
			}
		}
	}
	return points
}

func addPoints(t *testing.T, points map[string]int64, m metricdata.Metrics) {
	t.Helper()
	var dataPoints []metricdata.DataPoint[int64]
	switch data := m.Data.(type) {
	case metricdata.Sum[int64]:
		dataPoints = data.DataPoints
	case metricdata.Gauge[int64]:
		dataPoints = data.DataPoints
	default:
		t.Fatalf("%s: unexpected aggregation %T", m.Name, m.Data)
	}
	for _, p := range dataPoints {
		points[m.Name+"|"+p.Attributes.Encoded(attribute.DefaultEncoder())] = p.Value
	}
}

// TestReadAdmissionMetrics drives a shed at each gate and checks every instrument, with exactly
// the attribute sets spec §7.1 allows: none on the gauges, the gate alone on the shed counter.
func TestReadAdmissionMetrics(t *testing.T) {
	reader := recordMetrics(t)
	limits := testLimits()
	limits.ReadMemoryBytes = 64 << 20
	srv, s, tn := newServerWith(t, 1, limits)
	seed(t, s, tn, "metrics-ref", "deployment.started",
		time.Date(2026, 9, 14, 10, 0, 0, 0, time.UTC))
	h := srv.Routes()

	assert.Equal(t, map[string]int64{
		"horopter.read.admission.limit|":     1,
		"horopter.read.admission.in_flight|": 0,
		"horopter.read.memory.reserved|":     0,
		"horopter.read.memory.capacity|":     64 << 20,
	}, readMetricPoints(t, reader), "nothing is shed yet, and no gauge carries an attribute")

	finish := holdRead(t, h, changesURL(tn))
	assert.Equal(t, int64(1), readMetricPoints(t, reader)["horopter.read.admission.in_flight|"])
	require.Equal(t, http.StatusServiceUnavailable, get(t, h, changesURL(tn)).Code)
	require.Equal(t, http.StatusOK, finish().Code)

	release, err := egress.ReserveReadMemory(srv, limits.ReadMemoryBytes)
	require.NoError(t, err)
	assert.Equal(t, int64(64<<20), readMetricPoints(t, reader)["horopter.read.memory.reserved|"])
	require.Equal(t, http.StatusServiceUnavailable, get(t, h, changesURL(tn)).Code,
		"the seeded row needs memory, so a full pool must shed the read")
	release()

	points := readMetricPoints(t, reader)
	assert.Equal(t, int64(1), points["horopter.read.shed|horopter.read.shed.gate=concurrency"])
	assert.Equal(t, int64(1), points["horopter.read.shed|horopter.read.shed.gate=memory"])
	keys := make([]string, 0, len(points))
	for key := range points {
		keys = append(keys, key)
	}
	assert.ElementsMatch(t, []string{
		"horopter.read.admission.limit|",
		"horopter.read.admission.in_flight|",
		"horopter.read.memory.reserved|",
		"horopter.read.memory.capacity|",
		"horopter.read.shed|horopter.read.shed.gate=concurrency",
		"horopter.read.shed|horopter.read.shed.gate=memory",
	}, keys)
}
