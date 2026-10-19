package egress

import (
	"context"
	"log/slog"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/metric"
	"horopter.dev/horopter/internal/admission"
)

// meterScope is the instrumentation scope egress's read admission metrics are recorded under.
const meterScope = "horopter.dev/horopter/internal/egress"

type readMetrics struct {
	shed metric.Int64Counter
}

// Call once per process: the SDK's instrument cache would drop a second Server's gauges.
func newReadMetrics(limiter *admission.Limiter, memory *admission.BytePool) readMetrics {
	meter := otel.Meter(meterScope)
	shed, err := meter.Int64Counter("horopter.read.shed", metric.WithUnit("{read}"),
		metric.WithDescription("Reads refused because egress was at capacity, by gate"))
	logInstrumentError("horopter.read.shed", err)
	gauge(meter, "horopter.read.admission.limit", "{read}", "The read concurrency limit",
		func() int64 { return int64(limiter.Limit()) })
	gauge(meter, "horopter.read.admission.in_flight", "{read}",
		"Reads holding a concurrency permit", func() int64 { return int64(limiter.InFlight()) })
	gauge(meter, "horopter.read.memory.reserved", "By", "Read memory reserved by reads in flight",
		memory.Reserved)
	gauge(meter, "horopter.read.memory.capacity", "By", "Read memory reads may reserve in total",
		memory.Capacity)
	return readMetrics{shed: shed}
}

// gauge registers an observable gauge that reports value at every collection.
func gauge(meter metric.Meter, name, unit, description string, value func() int64) {
	_, err := meter.Int64ObservableGauge(name,
		metric.WithUnit(unit), metric.WithDescription(description),
		metric.WithInt64Callback(func(_ context.Context, o metric.Int64Observer) error {
			o.Observe(value())
			return nil
		}))
	logInstrumentError(name, err)
}

// The SDK returns a working instrument with the error, so the error means this code is wrong.
func logInstrumentError(name string, err error) {
	if err != nil {
		slog.Error("create a read admission instrument", "instrument", name, "error", err)
	}
}
