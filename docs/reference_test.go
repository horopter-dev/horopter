// Package docs holds no code: its tests keep the pages in docs/reference/ complete against the
// code they describe, failing in both directions — in the code but not documented, and
// documented but gone.
package docs

import (
	"crypto/rand"
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"horopter.dev/horopter/internal/auth"
	"horopter.dev/horopter/internal/egress"
	"horopter.dev/horopter/internal/ingress"
)

const moduleRoot = ".."

// envReaders are the functions a variable's name is passed to: the standard library's, the
// daemons' and egress's own helpers, and the env func horo and horopter-flux are handed so
// their tests can supply one. A new way of reading a variable is taught here in its own PR.
var envReaders = []string{"Getenv", "LookupEnv", "envOr", "durationFromEnv", "env"}

// testOnlyVar names variables only the tests read.
func testOnlyVar(name string) bool {
	return name == "HOROPTER_TEST_DSN" || strings.HasPrefix(name, "HORO_TEST_")
}

func TestConfigurationListsEveryVariable(t *testing.T) {
	inCode := map[string]bool{}
	for _, f := range parseModule(t) {
		for _, name := range variablesRead(f) {
			if !testOnlyVar(name) {
				inCode[name] = true
			}
		}
	}
	onPage := tableKeys(t, "configuration.md", regexp.MustCompile(`^[A-Z][A-Z0-9_]+$`))
	compare(t, "configuration.md", "variable", inCode, onPage)
}

// variablesRead returns the string literals f passes to an envReader, and the values of the
// constants it names …Var, as internal/logging's LevelVar and egress's readMemoryBytesVar are.
func variablesRead(f *ast.File) []string {
	var names []string
	ast.Inspect(f, func(n ast.Node) bool {
		switch n := n.(type) {
		case *ast.CallExpr:
			if slices.Contains(envReaders, calleeName(n)) && len(n.Args) > 0 {
				names = appendLit(names, n.Args[0])
			}
		case *ast.ValueSpec:
			for i, id := range n.Names {
				if strings.HasSuffix(id.Name, "Var") && i < len(n.Values) {
					names = appendLit(names, n.Values[i])
				}
			}
		}
		return true
	})
	return names
}

// appendLit appends e's value to names when e is a string literal.
func appendLit(names []string, e ast.Expr) []string {
	if s, ok := stringLit(e); ok {
		return append(names, s)
	}
	return names
}

// routeFiles are the files whose ServeMux registrations make each daemon's routes.
var routeFiles = map[string]string{
	"internal/ingress/server.go": "horopter-ingress",
	"internal/egress/server.go":  "horopter-egress",
}

var routePattern = regexp.MustCompile(`^(GET|POST|PUT|PATCH|DELETE) /`)

// route is one row of http-api.md's route table.
type route struct{ daemon, method, path string }

func (r route) String() string { return r.daemon + " " + r.method + " " + r.path }

func TestHTTPAPIListsEveryRoute(t *testing.T) {
	inCode := map[string]bool{}
	for file, daemon := range routeFiles {
		for _, pattern := range registrations(t, filepath.Join(moduleRoot, file)) {
			inCode[daemon+" "+pattern] = true
		}
	}
	onPage := map[string]bool{}
	for _, r := range documentedRoutes(t) {
		onPage[r.String()] = true
	}
	compare(t, "http-api.md", "route", inCode, onPage)
}

// TestHTTPAPIRoutesAreServed sends each documented route to its daemon's real handler. ServeMux
// answers 404 or 405 itself, before any handler runs, so any other answer means the route
// exists. No database is needed: every route but the probes refuses a request with no
// credential first, and readyz, which pings the store, panics on the nil one, which a missing
// route never does.
func TestHTTPAPIRoutesAreServed(t *testing.T) {
	clock := func() time.Time { return time.Unix(0, 0) }
	authn := auth.New(nil, clock, auth.DefaultOptions())
	in, err := ingress.New(nil, clock, rand.Reader, authn)
	if err != nil {
		t.Fatal(err)
	}
	out, err := egress.New(nil, clock, egress.DefaultLimits(), authn)
	if err != nil {
		t.Fatal(err)
	}
	handlers := map[string]http.Handler{
		"horopter-ingress": in.Routes(), "horopter-egress": out.Routes(),
	}
	for _, r := range documentedRoutes(t) {
		h, ok := handlers[r.daemon]
		if !ok {
			t.Errorf("http-api.md lists %s %s on %q, which is not a daemon", r.method, r.path,
				r.daemon)
			continue
		}
		if code, served := serve(h, r.method, r.path); !served {
			t.Errorf("http-api.md lists %s, which answers %d", r, code)
		}
	}
}

// serve reports whether h routed the request to a handler, recovering the panic the nil store
// raises in readyz.
func serve(h http.Handler, method, path string) (code int, served bool) {
	rec := httptest.NewRecorder()
	defer func() {
		if recover() != nil {
			code, served = 0, true
		}
	}()
	h.ServeHTTP(rec, httptest.NewRequest(method, path, nil))
	return rec.Code, rec.Code != http.StatusNotFound && rec.Code != http.StatusMethodNotAllowed
}

// documentedRoutes reads http-api.md's route table: a route in the first column, its daemon in
// the second. A route both daemons serve, as the probes are, has a row for each.
func documentedRoutes(t *testing.T) []route {
	t.Helper()
	var routes []route
	for _, row := range tableRows(t, "http-api.md") {
		pattern, ok := codeSpan(row[0])
		if !ok || !routePattern.MatchString(pattern) {
			continue
		}
		var daemon string
		if len(row) > 1 {
			daemon, _ = codeSpan(row[1])
		}
		method, path, _ := strings.Cut(pattern, " ")
		routes = append(routes, route{daemon: daemon, method: method, path: path})
	}
	return routes
}

// registrations returns the patterns a file passes to Handle and HandleFunc.
func registrations(t *testing.T, path string) []string {
	t.Helper()
	f := parseFile(t, path)
	var patterns []string
	ast.Inspect(f, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok || len(call.Args) == 0 {
			return true
		}
		if name := calleeName(call); name == "Handle" || name == "HandleFunc" {
			if s, ok := stringLit(call.Args[0]); ok {
				patterns = append(patterns, s)
			}
		}
		return true
	})
	return patterns
}

func TestHoroListsEverySubcommand(t *testing.T) {
	inCode := subcommands(t, "cmd/horo", "horo", map[string][]string{"run": nil})
	compare(t, "horo.md", "subcommand", inCode, commandHeadings(t, "horo.md", "horo"))
}

func TestHoroctlListsEverySubcommand(t *testing.T) {
	// compiledIn is a map rather than a switch, so its verbs are read from it as roots.
	inCode := subcommands(t, "cmd/horoctl", "horoctl", compiledIn(t))
	compare(t, "horoctl.md", "subcommand", inCode, commandHeadings(t, "horoctl.md", "horoctl"))
}

// subcommands expands a CLI's dispatch into its leaf commands. A dispatcher is a function that
// switches on args[0] and returns a call per case; roots maps each root dispatcher, or each
// verb a root map dispatches to, to the words before it.
func subcommands(t *testing.T, dir, binary string, roots map[string][]string) map[string]bool {
	t.Helper()
	dispatch := dispatchers(t, dir)
	leaves := map[string]bool{}
	var expand func(fn string, words []string)
	expand = func(fn string, words []string) {
		cases, ok := dispatch[fn]
		if !ok {
			leaves[strings.Join(append([]string{binary}, words...), " ")] = true
			return
		}
		for _, c := range cases {
			expand(c[1], append(slices.Clone(words), c[0]))
		}
	}
	for fn, words := range roots {
		expand(fn, words)
	}
	return leaves
}

// dispatchers maps each function in dir that switches on args[0] to its cases.
func dispatchers(t *testing.T, dir string) map[string][][2]string {
	t.Helper()
	dispatch := map[string][][2]string{}
	for _, f := range parseDir(t, filepath.Join(moduleRoot, dir)) {
		for _, d := range f.Decls {
			fn, ok := d.(*ast.FuncDecl)
			if !ok || fn.Body == nil {
				continue
			}
			if cases := switchOnFirstArg(fn.Body); len(cases) > 0 {
				dispatch[fn.Name.Name] = cases
			}
		}
	}
	return dispatch
}

// switchOnFirstArg returns each case of a `switch args[0]` in body as its word and the
// function its case returns a call to.
func switchOnFirstArg(body *ast.BlockStmt) [][2]string {
	var cases [][2]string
	ast.Inspect(body, func(n ast.Node) bool {
		sw, ok := n.(*ast.SwitchStmt)
		if !ok || !isFirstArg(sw.Tag) {
			return true
		}
		for _, stmt := range sw.Body.List {
			cc := stmt.(*ast.CaseClause)
			fn := returnedCall(cc.Body)
			for _, e := range cc.List {
				if word, ok := stringLit(e); ok && fn != "" {
					cases = append(cases, [2]string{word, fn})
				}
			}
		}
		return false
	})
	return cases
}

func isFirstArg(e ast.Expr) bool {
	idx, ok := e.(*ast.IndexExpr)
	if !ok {
		return false
	}
	id, ok := idx.X.(*ast.Ident)
	lit, isLit := idx.Index.(*ast.BasicLit)
	return ok && isLit && id.Name == "args" && lit.Value == "0"
}

func returnedCall(body []ast.Stmt) string {
	for _, stmt := range body {
		if ret, ok := stmt.(*ast.ReturnStmt); ok && len(ret.Results) == 1 {
			if call, ok := ret.Results[0].(*ast.CallExpr); ok {
				return calleeName(call)
			}
		}
	}
	return ""
}

// compiledIn reads horoctl's compiledIn map: each verb's function, rooted at that verb.
func compiledIn(t *testing.T) map[string][]string {
	t.Helper()
	roots := map[string][]string{}
	for _, f := range parseDir(t, filepath.Join(moduleRoot, "cmd/horoctl")) {
		ast.Inspect(f, func(n ast.Node) bool {
			spec, ok := n.(*ast.ValueSpec)
			if !ok || len(spec.Names) != 1 || spec.Names[0].Name != "compiledIn" {
				return true
			}
			for _, elt := range spec.Values[0].(*ast.CompositeLit).Elts {
				kv := elt.(*ast.KeyValueExpr)
				verb, _ := stringLit(kv.Key)
				roots[kv.Value.(*ast.Ident).Name] = []string{verb}
			}
			return false
		})
	}
	if len(roots) == 0 {
		t.Fatal("cmd/horoctl declares no compiledIn map; teach this test horoctl's dispatch")
	}
	return roots
}

// commandHeadings returns each heading on page that is a command in a code span, such as
// ## `horoctl admin keys issue`.
func commandHeadings(t *testing.T, page, binary string) map[string]bool {
	t.Helper()
	heading := regexp.MustCompile("^#{2,4} `(" + binary + "(?: [a-z][a-z-]*)+)`$")
	commands := map[string]bool{}
	for line := range strings.Lines(readPage(t, page)) {
		if m := heading.FindStringSubmatch(strings.TrimSpace(line)); m != nil {
			commands[m[1]] = true
		}
	}
	return commands
}

// instrumentCreators are the calls an instrument's name is passed to: the OpenTelemetry
// meter's constructors, and egress's gauge helper, which passes its name on to one.
var instrumentCreators = regexp.MustCompile(
	`^((Int64|Float64)(Observable)?(Counter|UpDownCounter|Histogram|Gauge)|gauge)$`)

func TestMetricsListsEveryInstrument(t *testing.T) {
	inCode := map[string]bool{}
	for _, f := range parseModule(t) {
		ast.Inspect(f, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok || !instrumentCreators.MatchString(calleeName(call)) {
				return true
			}
			for _, arg := range call.Args {
				if s, ok := stringLit(arg); ok && strings.HasPrefix(s, "horopter.") {
					inCode[s] = true
				}
			}
			return true
		})
	}
	onPage := tableKeys(t, "metrics.md", regexp.MustCompile(`^horopter\.[a-z_.]+$`))
	compare(t, "metrics.md", "instrument", inCode, onPage)
}

// compare fails for each name in the code and not on the page, and each on the page and not in
// the code.
func compare(t *testing.T, page, kind string, inCode, onPage map[string]bool) {
	t.Helper()
	if len(inCode) == 0 {
		t.Fatalf("found no %s in the code; the scan, not the code, is what changed", kind)
	}
	for _, name := range sortedKeys(inCode) {
		if !onPage[name] {
			t.Errorf("%s %s is in the code but not in docs/reference/%s", kind, name, page)
		}
	}
	for _, name := range sortedKeys(onPage) {
		if !inCode[name] {
			t.Errorf("docs/reference/%s lists %s %s, which the code no longer has", page, kind,
				name)
		}
	}
}

func sortedKeys(m map[string]bool) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	return keys
}

// tableKeys returns the first cell of every table row on page that is a code span whose text
// matches key.
func tableKeys(t *testing.T, page string, key *regexp.Regexp) map[string]bool {
	t.Helper()
	keys := map[string]bool{}
	for _, row := range tableRows(t, page) {
		if s, ok := codeSpan(row[0]); ok && key.MatchString(s) {
			keys[s] = true
		}
	}
	return keys
}

// tableRows returns the cells of every Markdown table row on page.
func tableRows(t *testing.T, page string) [][]string {
	t.Helper()
	var rows [][]string
	for line := range strings.Lines(readPage(t, page)) {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, "|") {
			continue
		}
		cells := strings.Split(strings.Trim(line, "|"), "|")
		for i := range cells {
			cells[i] = strings.TrimSpace(cells[i])
		}
		rows = append(rows, cells)
	}
	return rows
}

// codeSpan returns the text of a cell that is exactly one code span.
func codeSpan(cell string) (string, bool) {
	if len(cell) < 2 || cell[0] != '`' || cell[len(cell)-1] != '`' {
		return "", false
	}
	inner := cell[1 : len(cell)-1]
	return inner, !strings.Contains(inner, "`")
}

func readPage(t *testing.T, page string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("reference", page)) //nolint:gosec // G304: the pages.
	if err != nil {
		t.Fatalf("read docs/reference/%s: %v", page, err)
	}
	return string(b)
}

// parseModule parses every non-test Go file in the module, skipping hidden directories, which
// hold worktrees and tool state rather than the module's code.
func parseModule(t *testing.T) []*ast.File {
	t.Helper()
	var files []*ast.File
	err := filepath.WalkDir(moduleRoot, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() && path != moduleRoot && strings.HasPrefix(d.Name(), ".") {
			return filepath.SkipDir
		}
		if d.IsDir() || !isSource(d.Name()) {
			return nil
		}
		files = append(files, parseFile(t, path))
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return files
}

func parseDir(t *testing.T, dir string) []*ast.File {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	var files []*ast.File
	for _, e := range entries {
		if !e.IsDir() && isSource(e.Name()) {
			files = append(files, parseFile(t, filepath.Join(dir, e.Name())))
		}
	}
	return files
}

func isSource(name string) bool {
	return strings.HasSuffix(name, ".go") && !strings.HasSuffix(name, "_test.go")
}

func parseFile(t *testing.T, path string) *ast.File {
	t.Helper()
	f, err := parser.ParseFile(token.NewFileSet(), path, nil, parser.SkipObjectResolution)
	if err != nil {
		t.Fatal(err)
	}
	return f
}

// calleeName is the name a call is made by: f for f(…), and m for x.m(…).
func calleeName(call *ast.CallExpr) string {
	switch fn := call.Fun.(type) {
	case *ast.Ident:
		return fn.Name
	case *ast.SelectorExpr:
		return fn.Sel.Name
	}
	return ""
}

func stringLit(e ast.Expr) (string, bool) {
	lit, ok := e.(*ast.BasicLit)
	if !ok || lit.Kind != token.STRING {
		return "", false
	}
	s, err := strconv.Unquote(lit.Value)
	return s, err == nil
}
