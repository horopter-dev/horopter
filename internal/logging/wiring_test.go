package logging

import (
	"go/ast"
	"go/parser"
	"go/token"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestEveryBinaryInstallsTheHandlerBeforeTelemetry pins the startup order. telemetry.Setup
// passes slog.Default().Handler() to OpenTelemetry's internal logger, so a binary that calls
// it first gives OTel the text handler and keeps it for the life of the process — with
// nothing failing and no line out of place except OTel's own.
//
// cmd/horopter-migrate and horoctl's compiled-in migrate verb both call
// internal/migrate.Run rather than defining their own run(), so Run is where this
// ordering is enforced for both of them — one entry, not one per caller.
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
