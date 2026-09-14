package postgres_test

import (
	"os"
	"testing"
)

// dsn returns the test database DSN, skipping loudly when it is absent.
func dsn(t *testing.T) string {
	t.Helper()
	v := os.Getenv("HOROPTER_TEST_DSN")
	if v == "" {
		t.Skip("set HOROPTER_TEST_DSN to run Postgres tests (docker compose up -d db)")
	}
	return v
}
