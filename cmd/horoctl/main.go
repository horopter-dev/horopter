// Command horoctl manages horopter's tenants, credentials and migrations.
package main

import (
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"

	"horopter.dev/horopter/internal/buildinfo"
)

// compiledIn holds the verbs that must never be resolved from PATH.
var compiledIn = map[string]func([]string) error{
	"migrate": runMigrate,
	"admin":   runAdmin,
}

func main() {
	if code, handled := buildinfo.Handle("horoctl", os.Args[1:], os.Stdout, os.Stderr); handled {
		os.Exit(code)
	}
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

// exitDelegated exits with a plugin's own code, adding nothing to what it printed.
func exitDelegated(err error) {
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		os.Exit(exitErr.ExitCode())
	}
	fmt.Fprintln(os.Stderr, err)
	os.Exit(1)
}

// delegate runs horoctl-<verb> from PATH, telling it HOROCTL_VERSION.
func delegate(verb string, args []string) error {
	path, err := exec.LookPath("horoctl-" + verb)
	if err != nil {
		return fmt.Errorf("unknown command %q, and no horoctl-%s on PATH", verb, verb)
	}
	//nolint:gosec // G204: path is exec.LookPath's result; running a PATH plugin is the point.
	cmd := exec.Command(path, args...)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
	cmd.Env = append(os.Environ(), "HOROCTL_VERSION="+buildinfo.Version())
	return cmd.Run()
}

func usage(w io.Writer) {
	_, _ = fmt.Fprintln(w, "usage: horoctl <command> [args]")
	_, _ = fmt.Fprintln(w, "compiled in: migrate, admin")
	_, _ = fmt.Fprintln(w, "anything else runs horoctl-<command> from PATH")
	_, _ = fmt.Fprintln(w, "horoctl --version | --build-info [flags]: how this binary was built")
}
