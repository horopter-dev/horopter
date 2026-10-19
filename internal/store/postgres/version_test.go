package postgres_test

import (
	"context"
	"go/ast"
	"go/parser"
	"go/token"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"horopter.dev/horopter/internal/store/postgres"
)

func TestSchemaVersionIsRecordedByMigration(t *testing.T) {
	ctx, s := context.Background(), newStore(t)
	v, err := s.SchemaVersion(ctx)
	require.NoError(t, err)
	assert.Equal(t, postgres.ExpectedSchemaVersion, v)
	assert.NoError(t, s.AssertSchemaVersion(ctx))
}

// Isolated schema: an interrupted run would otherwise leave schema_version corrupted.
func TestAssertSchemaVersionFailsLoudlyOnMismatch(t *testing.T) {
	ctx := context.Background()
	s, err := postgres.Open(ctx, isolatedSchemaDSN(t, dsn(t)))
	require.NoError(t, err)
	t.Cleanup(s.Close)

	_, err = s.Pool().Exec(ctx, `UPDATE schema_version SET version = $1`, 99)
	require.NoError(t, err)

	err = s.AssertSchemaVersion(ctx)
	require.Error(t, err, "a daemon must refuse to run against an unexpected schema")
	assert.Contains(t, err.Error(), "99")
	assert.Contains(t, err.Error(), "horopter-migrate",
		"the error must tell the operator how to fix it")
}

// cmd/horopter-migrate is absent: it is the one binary that may migrate.
var daemonMainFiles = []string{
	"../../../cmd/horopter-ingress/main.go",
	"../../../cmd/horopter-egress/main.go",
}

// Also requires OpenNoMigrate and at least one postgres.* call, so it cannot pass vacuously.
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

// A ledger row the binary does not embed simulates a newer database.
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

	_, err = postgres.Migrate(ctx, s.Pool())
	require.NoError(t, err,
		"migrating a database ahead of this binary must not fail; it must record the truth")

	v, err := s.SchemaVersion(ctx)
	require.NoError(t, err)
	assert.Equal(t, 9999, v,
		"the recorded version must come from the ledger the database actually holds, not "+
			"from the migrations this binary embeds; recording the lower embedded number "+
			"leaves schema_version contradicting the applied_migration rows beside it")
}
