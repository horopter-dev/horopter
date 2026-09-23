package egress_test

import (
	"os"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/metric/noop"
	"horopter.dev/horopter/internal/core"
	"horopter.dev/horopter/internal/store/postgres"
)

// TestMain installs a real MeterProvider before any test in this package runs.
//
// go.opentelemetry.io/otel's global package deduplicates instruments by name, description and
// unit as each is created: before any real MeterProvider has ever been installed, only the
// first Server's gauges — the first newServerWith call in the whole test binary, through
// egress.New's otel.Meter(meterScope) call — are kept, and the first otel.SetMeterProvider call
// replays only those onto the provider it installs. A later Server's identical gauges, created
// once that provider is already live, are not added alongside them: the SDK meter's own
// instrument cache finds the name already registered and ignores the new callback, logging
// "Repeated observable instrument creation with callbacks" and leaving the gauge reporting the
// first Server's state forever.
//
// Without this TestMain, recordMetrics's own otel.SetMeterProvider call would be the first the
// test binary makes, so it would be the one earlier tests' Servers replay onto — carrying an
// already-finished test's gauge readings into a later test's ManualReader. Installing
// noop.NewMeterProvider() here first, before any Server exists, spends that one-time replay on
// an empty queue, so every later otel.SetMeterProvider call in this package (recordMetrics's)
// is a plain swap: the only gauges ever registered on a test's own provider are that test's own
// Server's.
func TestMain(m *testing.M) {
	otel.SetMeterProvider(noop.NewMeterProvider())
	os.Exit(m.Run())
}

// changeQuery and eventQuery build tenant-scoped store queries. The constructors refuse the
// zero core.TenantID, which no caller here passes, so the error is asserted once rather than
// at every site.
func changeQuery(t *testing.T, tn string, from, to time.Time, limit int) postgres.ChangeQuery {
	t.Helper()
	q, err := postgres.NewChangeQuery(core.TenantIDFromRow(tn), from, to, limit)
	require.NoError(t, err)
	return q
}

func eventQuery(t *testing.T, tn string, limit int) postgres.EventQuery {
	t.Helper()
	q, err := postgres.NewEventQuery(core.TenantIDFromRow(tn), limit)
	require.NoError(t, err)
	return q
}
