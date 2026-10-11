package main

import (
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"syscall"

	"horopter.dev/horopter/internal/core"
)

// outcome is how a wrapped command ended: its lifecycle, horo's exit code, and the
// attributes the end event carries.
type outcome struct {
	lifecycle core.Lifecycle
	exitCode  int
	attrs     map[string]any
}

var signalNames = map[syscall.Signal]string{
	syscall.SIGINT: "SIGINT", syscall.SIGTERM: "SIGTERM", syscall.SIGKILL: "SIGKILL",
	syscall.SIGHUP: "SIGHUP", syscall.SIGQUIT: "SIGQUIT",
}

// sharesTerminal reports whether a Ctrl-C at a terminal reaches horo's process group, and so
// the child's, directly. A variable so a test can pretend it does not.
var sharesTerminal = func() bool { return inForeground("/dev/tty") }

// forwarded are the signals horo passes on. On a terminal, Ctrl-C, Ctrl-\ and a hangup already
// reached the child's process group: forwarding them again would deliver a second one, which
// tools such as Terraform treat as "stop immediately". SIGTERM never comes from a terminal.
func forwarded(terminal bool) []os.Signal {
	if terminal {
		return []os.Signal{syscall.SIGTERM}
	}
	return []os.Signal{syscall.SIGTERM, os.Interrupt, syscall.SIGHUP, syscall.SIGQUIT}
}

// inherited records which signals were ignored when horo started — nohup leaves SIGHUP ignored,
// a script's background job SIGINT. It is read at package initialisation, before any Notify,
// which would un-ignore them.
var inherited = func() map[os.Signal]bool {
	ignored := map[os.Signal]bool{}
	for _, s := range []os.Signal{os.Interrupt, syscall.SIGTERM, syscall.SIGHUP, syscall.SIGQUIT} {
		ignored[s] = signal.Ignored(s)
	}
	return ignored
}()

// notify is signal.Notify for those of sigs horo did not inherit as ignored. Catching an ignored
// signal un-ignores it, and exec then gives the wrapped command the default action, ending the
// protection nohup set up. With none left it registers nothing: Notify with no signals means all.
// No call reaches that today: Go reports only SIGINT and SIGHUP as inherited-ignored, and every
// call includes SIGTERM. The guard is for the call that some day passes SIGINT alone.
func notify(c chan<- os.Signal, sigs ...os.Signal) {
	var keep []os.Signal
	for _, s := range sigs {
		if !inherited[s] {
			keep = append(keep, s)
		}
	}
	if len(keep) > 0 {
		signal.Notify(c, keep...)
	}
}

// runChild runs argv with stdin, stdout and stderr passed through, forwarding the given signals
// to it, and reports how it ended.
func runChild(argv []string, stdout, stderr io.Writer, forward []os.Signal) outcome {
	//nolint:gosec // G204: running the command its user names is what horo run is for.
	cmd := exec.Command(argv[0], argv[1:]...)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, stdout, stderr
	signals := make(chan os.Signal, 2)
	notify(signals, forward...)
	defer signal.Stop(signals)
	if err := cmd.Start(); err != nil {
		return outcome{lifecycle: core.LifecycleFailed, exitCode: 127,
			attrs: map[string]any{"error": err.Error()}}
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	for {
		select {
		case s := <-signals:
			_ = cmd.Process.Signal(s)
		case err := <-done:
			return classify(err)
		}
	}
}

func classify(err error) outcome {
	if err == nil {
		return outcome{lifecycle: core.LifecycleCompleted}
	}
	var exitErr *exec.ExitError
	if !errors.As(err, &exitErr) {
		return outcome{lifecycle: core.LifecycleFailed, exitCode: 1,
			attrs: map[string]any{"error": err.Error()}}
	}
	ws, ok := exitErr.Sys().(syscall.WaitStatus)
	if ok && ws.Signaled() {
		name, known := signalNames[ws.Signal()]
		if !known {
			name = fmt.Sprintf("signal %d", int(ws.Signal()))
		}
		return outcome{lifecycle: core.LifecycleCancelled, exitCode: 128 + int(ws.Signal()),
			attrs: map[string]any{"signal": name}}
	}
	code := exitErr.ExitCode()
	return outcome{lifecycle: core.LifecycleFailed, exitCode: code,
		attrs: map[string]any{"exit_code": code}}
}
