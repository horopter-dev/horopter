// Package testdb holds the shared-database helpers several test packages need.
package testdb

import (
	"context"
	"os"
	"sync"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
)

// DSNVar names the environment variable every database test reads its DSN from.
const DSNVar = "HOROPTER_TEST_DSN"

// pool is shared by every cleanup: a test's own store is closed by its own t.Cleanup.
var (
	pool     *pgxpool.Pool
	poolOnce sync.Once
	poolErr  error
)

// Pool returns the shared cleanup pool, opening it on first use.
func Pool() (*pgxpool.Pool, error) {
	poolOnce.Do(func() {
		pool, poolErr = pgxpool.New(context.Background(), os.Getenv(DSNVar))
	})
	return pool, poolErr
}

// NewTenant creates a tenant row for id, removed with its events and changes after the test.
func NewTenant(t *testing.T, id string) string {
	t.Helper()
	p, err := Pool()
	if err != nil {
		t.Fatalf("open pool to seed tenant %q: %v", id, err)
	}
	if _, err := p.Exec(context.Background(),
		`INSERT INTO tenant (id, display_name) VALUES ($1, $1)`, id); err != nil {
		t.Fatalf("seed tenant %q: %v", id, err)
	}
	// Registered first so it runs last, after the rows the foreign keys hold.
	t.Cleanup(func() { exec(t, `DELETE FROM tenant WHERE id = $1`, "delete tenant", id) })
	DropTenantAfter(t, id)
	return id
}

// EnsureTenant makes id exist as a tenant row, for a shared tenant that is never dropped.
func EnsureTenant(t *testing.T, id string) string {
	t.Helper()
	p, err := Pool()
	if err != nil {
		t.Fatalf("open pool to seed tenant %q: %v", id, err)
	}
	if _, err := p.Exec(context.Background(),
		`INSERT INTO tenant (id, display_name) VALUES ($1, $1) ON CONFLICT (id) DO NOTHING`,
		id); err != nil {
		t.Fatalf("seed tenant %q: %v", id, err)
	}
	return id
}

// DropTenantAfter deletes the tenant's rows after the test; a failure is logged, not fatal.
func DropTenantAfter(t *testing.T, tenant string) {
	t.Helper()
	t.Cleanup(func() {
		exec(t, `DELETE FROM change WHERE tenant = $1`, "delete changes", tenant)
		exec(t, `DELETE FROM event WHERE tenant = $1`, "delete events", tenant)
	})
}

func exec(t *testing.T, sql, what string, args ...any) {
	t.Helper()
	p, err := Pool()
	if err != nil {
		t.Logf("tenant cleanup (%s) for %v: open pool: %v", what, args, err)
		return
	}
	if _, err := p.Exec(context.Background(), sql, args...); err != nil {
		t.Logf("tenant cleanup (%s) for %v: %v", what, args, err)
	}
}
