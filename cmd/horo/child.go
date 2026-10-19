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

// outcome is how a wrapped command ended, with the end event's attributes.
type outcome struct {
	lifecycle core.Lifecycle
	exitCode  int
	attrs     map[string]any
}

var signalNames = map[syscall.Signal]string{
	syscall.SIGINT: "SIGINT", syscall.SIGTERM: "SIGTERM", syscall.SIGKILL: "SIGKILL",
	syscall.SIGHUP: "SIGHUP", syscall.SIGQUIT: "SIGQUIT",
}

// sharesTerminal reports whether a terminal's Ctrl-C reaches the child directly.
var sharesTerminal = func() bool { return inForeground("/dev/tty") }

// forwarded are the signals horo passes on: on a terminal, the others reached the child already.
func forwarded(terminal bool) []os.Signal {
	if terminal {
		return []os.Signal{syscall.SIGTERM}
	}
	return []os.Signal{syscall.SIGTERM, os.Interrupt, syscall.SIGHUP, syscall.SIGQUIT}
}

// inherited are the signals ignored at start-up, read before any Notify un-ignores them.
var inherited = func() map[os.Signal]bool {
	ignored := map[os.Signal]bool{}
	for _, s := range []os.Signal{os.Interrupt, syscall.SIGTERM, syscall.SIGHUP, syscall.SIGQUIT} {
		ignored[s] = signal.Ignored(s)
	}
	return ignored
}()

// notify is signal.Notify without inherited-ignored signals; Notify with none catches all.
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

// runChild runs argv with stdio passed through, forwarding forward, and reports how it ended.
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
