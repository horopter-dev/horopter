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

// TestMain installs a noop MeterProvider first, so otel's one-time replay leaks no gauges.
func TestMain(m *testing.M) {
	otel.SetMeterProvider(noop.NewMeterProvider())
	os.Exit(m.Run())
}

func changeQuery(
	t *testing.T, tn string, from, to time.Time, limit int, attrs map[string]string,
) postgres.ChangeQuery {
	t.Helper()
	q, err := postgres.NewChangeQuery(core.TenantIDFromRow(tn), from, to, limit, attrs)
	require.NoError(t, err)
	return q
}

func eventQuery(t *testing.T, tn string, limit int) postgres.EventQuery {
	t.Helper()
	q, err := postgres.NewEventQuery(core.TenantIDFromRow(tn), limit)
	require.NoError(t, err)
	return q
}
