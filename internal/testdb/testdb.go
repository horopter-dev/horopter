// Package testdb holds the shared-database helpers the test suites need.
//
// It is a normal package rather than a _test.go file because four test packages use it and
// none of them can import another's test files. Nothing outside a test imports it, so
// depending on testing here costs a production binary nothing — the same arrangement
// net/http/httptest uses.
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

// pool is one connection pool shared by every cleanup in the test binary. A pool per test
// would pay a connect for two DELETEs, and the tests' own stores cannot be reused: a store
// is closed by its own t.Cleanup, and only the LIFO ordering of a single test keeps that
// until after the delete.
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

// NewTenant creates a tenant row for id and registers its own removal alongside its
// events and changes, so a test tenant is left exactly as it was found. Migration 0005
// added event_tenant_fk and change_tenant_fk, so any test that writes under a tenant
// string now has to make that string a real row first — that is the constraint doing
// its job, not a fixture problem.
//
// Registration order matters. t.Cleanup runs LIFO, and this registers the tenant row's
// own deletion before calling DropTenantAfter, so DropTenantAfter's events-and-changes
// cleanup — registered second — runs first, ahead of the row the foreign keys point to.
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
	t.Cleanup(func() { exec(t, `DELETE FROM tenant WHERE id = $1`, "delete tenant", id) })
	DropTenantAfter(t, id)
	return id
}

// EnsureTenant makes id exist as a tenant row without registering any cleanup, for a
// fixed, shared tenant that other tests and other runs also write into and that must not
// be dropped out from under them.
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

// DropTenantAfter deletes the tenant's rows once the test ends, so a suite run leaves the
// shared database as it found it. Before this existed, every run wrote a few hundred rows
// under per-test tenants that nothing removed; they accumulated across months of runs until
// the seeded rows an operator wanted to look at were a rounding error.
//
// This is hygiene, not isolation. Tests still pick identifiers no earlier run used and
// still must not assume the database is otherwise empty: a killed test binary skips
// cleanup, and other packages' tenants are present throughout.
//
// Failures are logged, not fatal. Residue is exactly the condition being removed, so
// failing an otherwise passing test over it would trade a tidy database for a flaky suite.
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
