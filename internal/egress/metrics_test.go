package egress_test

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
	"horopter.dev/horopter/internal/egress"
)

// Call before egress.New, which takes its instruments from the global provider.
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

// Keyed "name|attrs", e.g. "horopter.read.shed|horopter.read.shed.gate=memory".
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

type instrumentShape struct {
	scope, unit, kind string
}

func wantShape(unit, kind string) instrumentShape {
	return instrumentShape{
		scope: "horopter.dev/horopter/internal/egress", unit: unit, kind: kind,
	}
}

func readMetricShapes(t *testing.T, reader *sdkmetric.ManualReader) map[string]instrumentShape {
	t.Helper()
	var rm metricdata.ResourceMetrics
	require.NoError(t, reader.Collect(context.Background(), &rm))
	shapes := map[string]instrumentShape{}
	for _, scope := range rm.ScopeMetrics {
		for _, m := range scope.Metrics {
			if strings.HasPrefix(m.Name, "horopter.read.") {
				shapes[m.Name] = instrumentShape{scope: scope.Scope.Name, unit: m.Unit, kind: kindOf(m)}
			}
		}
	}
	return shapes
}

// A non-monotonic "sum" matches no wanted shape, so a counter built as one is caught.
func kindOf(m metricdata.Metrics) string {
	switch data := m.Data.(type) {
	case metricdata.Gauge[int64]:
		return "gauge"
	case metricdata.Sum[int64]:
		if data.IsMonotonic {
			return "monotonic_sum"
		}
		return "sum"
	default:
		return fmt.Sprintf("unexpected aggregation %T", m.Data)
	}
}

func TestReadAdmissionMetrics(t *testing.T) {
	reader := recordMetrics(t)
	limits := testLimits()
	limits.ReadMemoryBytes = 64 << 20
	srv, s, tn := newServerWith(t, 1, limits)
	key := readerKey(t, s, tn)
	seed(t, s, tn, "ci/metrics-ref", "deployment.started",
		time.Date(2026, 9, 14, 10, 0, 0, 0, time.UTC))
	h := srv.Routes()

	assert.Equal(t, map[string]int64{
		"horopter.read.admission.limit|":     1,
		"horopter.read.admission.in_flight|": 0,
		"horopter.read.memory.reserved|":     0,
		"horopter.read.memory.capacity|":     64 << 20,
	}, readMetricPoints(t, reader), "nothing is shed yet, and no gauge carries an attribute")

	finish := holdRead(t, h, key, changesURL(tn))
	assert.Equal(t, int64(1), readMetricPoints(t, reader)["horopter.read.admission.in_flight|"])
	require.Equal(t, http.StatusServiceUnavailable, get(t, h, key, changesURL(tn)).Code)
	require.Equal(t, http.StatusOK, finish().Code)

	release, err := egress.ReserveReadMemory(srv, limits.ReadMemoryBytes)
	require.NoError(t, err)
	assert.Equal(t, int64(64<<20), readMetricPoints(t, reader)["horopter.read.memory.reserved|"])
	require.Equal(t, http.StatusServiceUnavailable, get(t, h, key, changesURL(tn)).Code,
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

	assert.Equal(t, map[string]instrumentShape{
		"horopter.read.admission.limit":     wantShape("{read}", "gauge"),
		"horopter.read.admission.in_flight": wantShape("{read}", "gauge"),
		"horopter.read.memory.reserved":     wantShape("By", "gauge"),
		"horopter.read.memory.capacity":     wantShape("By", "gauge"),
		"horopter.read.shed":                wantShape("{read}", "monotonic_sum"),
	}, readMetricShapes(t, reader),
		"the exact scope, unit and kind for every read admission instrument")
}
