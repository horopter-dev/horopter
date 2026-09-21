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

// readMetrics are the instruments read admission reports through. Only the shed counter is
// recorded from a request; the gauges read the gates whenever metrics are collected.
type readMetrics struct {
	shed metric.Int64Counter
}

// newReadMetrics creates egress's read admission instruments from the global meter provider as
// it stands, which is why horopter-egress calls telemetry.Setup before egress.New. No instrument
// carries a tenant or anything else a caller chooses. An instrument the SDK reports an error for
// is logged, and egress serves reads regardless.
//
// Each gauge's callback is registered once, when its instrument is created here — not
// re-registered on every collection. A second call to newReadMetrics against the same meter
// provider would register a second Server's gauges under the same names, units and
// descriptions, and the SDK's own instrument cache would keep only the first: the second
// Server's callbacks are dropped (logged as "Repeated observable instrument creation with
// callbacks"), and every gauge goes on reporting the first Server's gates forever. This is safe
// because horopter-egress's main creates exactly one Server, hence exactly one call here, per
// process.
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

// logInstrumentError logs an instrument the SDK could not create cleanly. The SDK returns a
// working instrument alongside a name-validation error, so the error means this code is wrong,
// not the environment.
func logInstrumentError(name string, err error) {
	if err != nil {
		slog.Error("create a read admission instrument", "instrument", name, "error", err)
	}
}
