package logging

import (
	"go/ast"
	"go/parser"
	"go/token"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Both migrate entry points call internal/migrate.Run, so Run is checked for both.
func TestEveryBinaryInstallsTheHandlerBeforeTelemetry(t *testing.T) {
	for _, c := range []struct{ path, fn string }{
		{"../../cmd/horopter-ingress/main.go", "run"},
		{"../../cmd/horopter-egress/main.go", "run"},
		{"../../internal/migrate/migrate.go", "Run"},
	} {
		file, err := parser.ParseFile(token.NewFileSet(), c.path, nil, parser.SkipObjectResolution)
		require.NoError(t, err)

		calls := callsInFunc(file, c.fn)
		logging := indexOf(calls, "logging.Setup")
		telemetry := indexOf(calls, "telemetry.Setup")

		require.NotEqual(t, -1, logging, "%s: %s() never calls logging.Setup", c.path, c.fn)
		require.NotEqual(t, -1, telemetry, "%s: %s() never calls telemetry.Setup", c.path, c.fn)
		assert.Equal(t, 0, logging, "%s: logging.Setup must be %s()'s first call", c.path, c.fn)
		assert.Less(t, logging, telemetry,
			"%s: logging.Setup must come before telemetry.Setup", c.path)
	}
}

// callsInFunc returns the qualified names of the calls in name's body, in source order.
func callsInFunc(file *ast.File, name string) []string {
	var calls []string
	ast.Inspect(file, func(n ast.Node) bool {
		fn, ok := n.(*ast.FuncDecl)
		if !ok || fn.Name.Name != name {
			return true
		}
		ast.Inspect(fn.Body, func(inner ast.Node) bool {
			call, ok := inner.(*ast.CallExpr)
			if !ok {
				return true
			}
			if sel, ok := call.Fun.(*ast.SelectorExpr); ok {
				if pkg, ok := sel.X.(*ast.Ident); ok {
					calls = append(calls, pkg.Name+"."+sel.Sel.Name)
				}
			}
			return true
		})
		return false
	})
	return calls
}

func indexOf(names []string, want string) int {
	for i, name := range names {
		if name == want {
			return i
		}
	}
	return -1
}
