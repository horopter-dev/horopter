// Package buildinfo reports how a binary was built and answers --version and --build-info.
package buildinfo

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"runtime"
	"runtime/debug"
	"strconv"
	"strings"
)

const (
	exitOK    = 0
	exitError = 1
	exitUsage = 2
)

// version is the release GoReleaser stamps via -ldflags, without the tag's v.
var version string

// readBuildInfo is a seam for tests.
var readBuildInfo = debug.ReadBuildInfo

// Version is the stamped release, else the module version without its v, or "unknown".
func Version() string {
	if version != "" {
		return version
	}
	if info, ok := readBuildInfo(); ok && info.Main.Version != "" {
		return strings.TrimPrefix(info.Main.Version, "v")
	}
	return "unknown"
}

// display is how text output shows a version: a release gets back the v its tag carries.
func display(v string) string {
	if v != "" && v[0] >= '0' && v[0] <= '9' {
		return "v" + v
	}
	return v
}

// Handle answers --version or --build-info at the start of args; code is the exit status.
func Handle(name string, args []string, stdout, stderr io.Writer) (code int, handled bool) {
	if len(args) == 0 || !isInfoFlag(args[0]) {
		return exitOK, false
	}
	q, err := parse(name, args, stderr)
	if err != nil {
		if !errors.Is(err, errReported) {
			_, _ = fmt.Fprintf(stderr, "%s: %v\n", name, err)
		}
		return exitUsage, true
	}
	var r report = versionReport{Name: name, Version: Version()}
	if q.buildInfo {
		r = newBuildReport(name)
	}
	if err := write(stdout, q.format, r); err != nil {
		_, _ = fmt.Fprintf(stderr, "%s: write %s: %v\n", name, args[0], err)
		return exitError, true
	}
	return exitOK, true
}

func isInfoFlag(arg string) bool {
	switch arg {
	case "--version", "-version", "--build-info", "-build-info":
		return true
	}
	return false
}

type query struct {
	buildInfo bool
	format    string
}

// errReported is a parse error the FlagSet has already written to stderr.
var errReported = errors.New("reported")

func parse(name string, args []string, stderr io.Writer) (query, error) {
	fs := flag.NewFlagSet(name, flag.ContinueOnError)
	fs.SetOutput(stderr)
	var q query
	showVersion := fs.Bool("version", false, "print the release this binary was built from")
	fs.BoolVar(&q.buildInfo, "build-info", false, "print the release and how it was built")
	fs.StringVar(&q.format, "format", "text", "text, json, or bare: the version alone")
	asJSON := fs.Bool("json", false, "shorthand for --format json")
	if err := fs.Parse(args); err != nil {
		return q, errReported
	}
	if fs.NArg() > 0 {
		return q, fmt.Errorf("%s takes no arguments, got %q", args[0], fs.Args())
	}
	return q, resolve(&q, fs, *showVersion, *asJSON)
}

// resolve settles the flags that conflict with or abbreviate one another.
func resolve(q *query, fs *flag.FlagSet, showVersion, asJSON bool) error {
	if showVersion && q.buildInfo {
		return errors.New("give --version or --build-info, not both")
	}
	if asJSON {
		if formatSet(fs) {
			return errors.New("give --json or --format, not both")
		}
		q.format = "json"
	}
	return checkFormat(*q)
}

var formats = map[string]bool{"text": true, "json": true, "bare": true}

func checkFormat(q query) error {
	if !formats[q.format] {
		return fmt.Errorf("--format %q: use text, json or bare", q.format)
	}
	if q.format == "bare" && q.buildInfo {
		return errors.New("--format bare goes with --version, not --build-info")
	}
	return nil
}

func formatSet(fs *flag.FlagSet) bool {
	set := false
	fs.Visit(func(f *flag.Flag) { set = set || f.Name == "format" })
	return set
}

type report interface{ writeText(io.Writer) error }

func write(w io.Writer, format string, r report) error {
	switch format {
	case "json":
		return json.NewEncoder(w).Encode(r)
	case "bare":
		_, err := fmt.Fprintln(w, Version())
		return err
	}
	return r.writeText(w)
}

type versionReport struct {
	Name    string `json:"name"`
	Version string `json:"version"`
}

func (r versionReport) writeText(w io.Writer) error {
	_, err := fmt.Fprintf(w, "%s %s\n", r.Name, display(r.Version))
	return err
}

// buildReport is what --build-info reports; the commit time is the commit's, not the build's.
type buildReport struct {
	Name       string  `json:"name"`
	Version    string  `json:"version"`
	Commit     *string `json:"commit"`
	CommitTime *string `json:"commit_time"`
	Modified   *bool   `json:"modified"`
	GoVersion  string  `json:"go_version"`
	OS         string  `json:"os"`
	Arch       string  `json:"arch"`
}

func newBuildReport(name string) buildReport {
	r := buildReport{
		Name: name, Version: Version(),
		GoVersion: runtime.Version(), OS: runtime.GOOS, Arch: runtime.GOARCH,
	}
	info, ok := readBuildInfo()
	if !ok {
		return r
	}
	for _, s := range info.Settings {
		switch s.Key {
		case "vcs.revision":
			r.Commit = new(s.Value)
		case "vcs.time":
			r.CommitTime = new(s.Value)
		case "vcs.modified":
			r.Modified = new(s.Value == "true")
		}
	}
	return r
}

func (r buildReport) writeText(w io.Writer) error {
	if err := (versionReport{Name: r.Name, Version: r.Version}).writeText(w); err != nil {
		return err
	}
	var modified *string
	if r.Modified != nil {
		modified = new(strconv.FormatBool(*r.Modified))
	}
	lines := []struct {
		label string
		value *string
	}{
		{"Commit", r.Commit},
		{"Commit time", r.CommitTime},
		{"Modified", modified},
		{"Go version", &r.GoVersion},
		{"OS", &r.OS},
		{"Arch", &r.Arch},
	}
	for _, l := range lines {
		if l.value == nil {
			continue
		}
		if _, err := fmt.Fprintf(w, "%-12s %s\n", l.label+":", *l.value); err != nil {
			return err
		}
	}
	return nil
}
