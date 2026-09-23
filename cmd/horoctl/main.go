// Command horoctl is the management CLI for horopter: it creates tenants, issues and
// revokes credentials, and runs migrations.
//
// It follows kubectl's dispatch model. migrate and admin touch the database schema or the
// identity tables that gate every write to it, so they are compiled in and version-locked
// rather than resolved from PATH: a PATH-dispatched launcher could exec a
// differently-versioned plugin, and cmd/horoctl-migrate/main.go's package comment records
// why that would matter for migrate specifically. Because every schema-touching verb is
// compiled in, the launcher can never exec a different build for one — the fallthrough
// below only ever reaches verbs that do not exist in this repository.
package main

import (
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
)

// version is passed to a delegated plugin as HOROCTL_VERSION, so it can check
// compatibility with the horoctl that invoked it.
var version = "dev"

// compiledIn holds the verbs that must never be resolved from PATH.
var compiledIn = map[string]func([]string) error{
	"migrate": runMigrate,
	"admin":   runAdmin,
}

func main() {
	if len(os.Args) < 2 {
		usage(os.Stderr)
		os.Exit(2)
	}
	verb := os.Args[1]
	if run, ok := compiledIn[verb]; ok {
		if err := run(os.Args[2:]); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		return
	}
	if err := delegate(verb, os.Args[2:]); err != nil {
		exitDelegated(err)
	}
}

// exitDelegated exits with the delegated plugin's own exit code when err reports one, so
// a caller scripting `horoctl weather; case $? in 3) ...` sees exactly what the plugin
// returned. The plugin's stdout and stderr were already connected straight through in
// delegate, so nothing more is printed here — a wrapper message on top of what the plugin
// already reported would be a second, redundant account of the same failure. Only an error
// delegate raised itself (an unknown verb, a plugin that could not be started at all) is
// horoctl's own to report, and gets the generic exit 1.
func exitDelegated(err error) {
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		os.Exit(exitErr.ExitCode())
	}
	fmt.Fprintln(os.Stderr, err)
	os.Exit(1)
}

// delegate runs horoctl-<verb> from PATH. Schema-touching verbs are in compiledIn and are
// looked up first, so no plugin can answer for one at a different version. path comes from
// exec.LookPath against PATH, not from unvalidated caller input — the kubectl-style plugin
// model this launcher follows is exactly "execute a binary named horoctl-<verb> found on
// PATH".
//
//nolint:gosec // G204
func delegate(verb string, args []string) error {
	path, err := exec.LookPath("horoctl-" + verb)
	if err != nil {
		return fmt.Errorf("unknown command %q, and no horoctl-%s on PATH", verb, verb)
	}
	cmd := exec.Command(path, args...)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
	cmd.Env = append(os.Environ(), "HOROCTL_VERSION="+version)
	return cmd.Run()
}

func usage(w io.Writer) {
	_, _ = fmt.Fprintln(w, "usage: horoctl <command> [args]")
	_, _ = fmt.Fprintln(w, "compiled in: migrate, admin")
	_, _ = fmt.Fprintln(w, "anything else runs horoctl-<command> from PATH")
}
