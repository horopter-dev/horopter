package postgres_test

import (
	"context"
	"go/ast"
	"go/parser"
	"go/token"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/yo61/horopter/internal/store/postgres"
)

func TestSchemaVersionIsRecordedByMigration(t *testing.T) {
	ctx, s := context.Background(), newStore(t)
	v, err := s.SchemaVersion(ctx)
	require.NoError(t, err)
	assert.Equal(t, postgres.ExpectedSchemaVersion, v)
	assert.NoError(t, s.AssertSchemaVersion(ctx))
}

func TestAssertSchemaVersionFailsLoudlyOnMismatch(t *testing.T) {
	ctx, s := context.Background(), newStore(t)

	_, err := s.Pool().Exec(ctx, `UPDATE schema_version SET version = $1`, 99)
	require.NoError(t, err)
	t.Cleanup(func() {
		_, _ = s.Pool().Exec(ctx, `UPDATE schema_version SET version = $1`,
			postgres.ExpectedSchemaVersion)
	})

	err = s.AssertSchemaVersion(ctx)
	require.Error(t, err, "a daemon must refuse to run against an unexpected schema")
	assert.Contains(t, err.Error(), "99")
	assert.Contains(t, err.Error(), "horopter-migrate",
		"the error must tell the operator how to fix it")
}

// daemonMainFiles are the daemons that must never apply DDL: several replicas start
// together on every rolling deploy, and the only safe number of processes migrating
// is one. cmd/horopter-migrate/main.go is deliberately absent from this list — it is
// the one binary that must keep calling postgres.Open.
var daemonMainFiles = []string{
	"../../../cmd/horopter-ingress/main.go",
	"../../../cmd/horopter-egress/main.go",
}

// TestDaemonsNeverMigrateOnStartup is the structural counterpart to
// TestRoutesRegisterOnlyGETHandlers in internal/egress/server_test.go: no test
// anywhere under cmd/ exercises a daemon's startup wiring against a real database, and
// no configured linter has a rule against calling postgres.Open outside
// horopter-migrate, so nothing previously caught a daemon being reverted to the
// migrating constructor. This parses each daemon's main.go and inspects every
// postgres.* call it makes.
//
// A bare "must not call postgres.Open" assertion would pass a file that opens no
// store at all, so it also requires the file to call postgres.OpenNoMigrate — and,
// because a broken path or a renamed import could make the walk silently see nothing,
// it fails loudly if it finds zero postgres.* calls to begin with, rather than
// reporting safety it never actually checked.
func TestDaemonsNeverMigrateOnStartup(t *testing.T) {
	for _, path := range daemonMainFiles {
		t.Run(path, func(t *testing.T) {
			fset := token.NewFileSet()
			file, err := parser.ParseFile(fset, path, nil, parser.SkipObjectResolution)
			require.NoError(t, err, "parse %s to inspect its postgres.* calls", path)

			var calls []string
			ast.Inspect(file, func(n ast.Node) bool {
				call, ok := n.(*ast.CallExpr)
				if !ok {
					return true
				}
				sel, ok := call.Fun.(*ast.SelectorExpr)
				if !ok {
					return true
				}
				pkg, ok := sel.X.(*ast.Ident)
				if !ok || pkg.Name != "postgres" {
					return true
				}
				calls = append(calls, sel.Sel.Name)
				return true
			})

			require.NotEmpty(t, calls, "%s: found no postgres.* calls; this guard checked "+
				"nothing — has the store package's import name or path changed?", path)

			for _, name := range calls {
				assert.NotEqual(t, "Open", name,
					"%s calls postgres.Open, which applies migrations; daemons must call "+
						"postgres.OpenNoMigrate instead, because only horopter-migrate may "+
						"change the schema", path)
			}
			assert.Contains(t, calls, "OpenNoMigrate",
				"%s never calls postgres.OpenNoMigrate; a daemon that opens no store at all "+
					"would otherwise pass this guard vacuously", path)
		})
	}
}

// TestMigrateDoesNotDowngradeARecordedSchemaVersion covers running an OLD
// horopter-migrate against a NEWER database, which happens on any rollback or skewed
// rollout. The recorded version used to come from the highest migration the BINARY
// embeds, so the old binary rewrote schema_version downward while applied_migration still
// listed every file actually applied — the recorded version then contradicted the
// database's real structure and nothing raised a word.
//
// A ledger row the binary does not embed is what simulates the newer database: this
// binary's embedded set stops well below 9999, so an embedded-list derivation records its
// own lower number and a ledger derivation records 9999.
//
// It runs against an isolated schema, not the shared public one: it writes a bogus ledger
// row and a version no daemon would accept, and leaving either behind in public would make
// every later run of the suite fail.
func TestMigrateDoesNotDowngradeARecordedSchemaVersion(t *testing.T) {
	ctx := context.Background()
	s, err := postgres.Open(ctx, isolatedSchemaDSN(t, dsn(t)))
	require.NoError(t, err)
	t.Cleanup(s.Close)

	_, err = s.Pool().Exec(ctx,
		`INSERT INTO applied_migration (filename) VALUES ('9999_applied_by_a_newer_binary.sql')`)
	require.NoError(t, err)
	_, err = s.Pool().Exec(ctx, `UPDATE schema_version SET version = 9999`)
	require.NoError(t, err)

	require.NoError(t, postgres.Migrate(ctx, s.Pool()),
		"migrating a database ahead of this binary must not fail; it must record the truth")

	v, err := s.SchemaVersion(ctx)
	require.NoError(t, err)
	assert.Equal(t, 9999, v,
		"the recorded version must come from the ledger the database actually holds, not "+
			"from the migrations this binary embeds; recording the lower embedded number "+
			"leaves schema_version contradicting the applied_migration rows beside it")
}
