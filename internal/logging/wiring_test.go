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
func TestEveryBinaryInstallsTheHandlerBeforeTelemetry(t *testing.T) {
	for _, path := range []string{
		"../../cmd/horopter-ingress/main.go",
		"../../cmd/horopter-egress/main.go",
		"../../cmd/horopter-migrate/main.go",
	} {
		file, err := parser.ParseFile(token.NewFileSet(), path, nil, parser.SkipObjectResolution)
		require.NoError(t, err)

		calls := callsInRun(file)
		logging := indexOf(calls, "logging.Setup")
		telemetry := indexOf(calls, "telemetry.Setup")

		require.NotEqual(t, -1, logging, "%s: run() never calls logging.Setup", path)
		require.NotEqual(t, -1, telemetry, "%s: run() never calls telemetry.Setup", path)
		assert.Equal(t, 0, logging, "%s: logging.Setup must be run()'s first call", path)
		assert.Less(t, logging, telemetry,
			"%s: logging.Setup must come before telemetry.Setup", path)
	}
}

// callsInRun returns the qualified names of the calls in run's body, in source order.
func callsInRun(file *ast.File) []string {
	var calls []string
	ast.Inspect(file, func(n ast.Node) bool {
		fn, ok := n.(*ast.FuncDecl)
		if !ok || fn.Name.Name != "run" {
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
