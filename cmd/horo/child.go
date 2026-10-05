package main

import (
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"syscall"
)

// outcome is how a wrapped command ended: the lifecycle word, horo's exit code, and the
// attributes the end event carries.
type outcome struct {
	word     string
	exitCode int
	attrs    map[string]any
}

var signalNames = map[syscall.Signal]string{
	syscall.SIGINT: "SIGINT", syscall.SIGTERM: "SIGTERM", syscall.SIGKILL: "SIGKILL",
	syscall.SIGHUP: "SIGHUP", syscall.SIGQUIT: "SIGQUIT",
}

// sharesTerminal reports whether a Ctrl-C at a terminal reaches horo's process group, and so
// the child's, directly. A variable so a test can pretend it does not.
var sharesTerminal = func() bool { return inForeground("/dev/tty") }

// forwarded are the signals horo passes on. On a terminal, Ctrl-C already reached the child's
// process group: forwarding it again would deliver a second interrupt, which tools such as
// Terraform treat as "stop immediately".
func forwarded(terminal bool) []os.Signal {
	if terminal {
		return []os.Signal{syscall.SIGTERM}
	}
	return []os.Signal{syscall.SIGTERM, os.Interrupt}
}

// runChild runs argv with stdin, stdout and stderr passed through, forwarding the given signals
// to it, and reports how it ended.
func runChild(argv []string, stdout, stderr io.Writer, forward []os.Signal) outcome {
	//nolint:gosec // G204: running the command its user names is what horo run is for.
	cmd := exec.Command(argv[0], argv[1:]...)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, stdout, stderr
	signals := make(chan os.Signal, 2)
	signal.Notify(signals, forward...)
	defer signal.Stop(signals)
	if err := cmd.Start(); err != nil {
		return outcome{word: "failed", exitCode: 127, attrs: map[string]any{"error": err.Error()}}
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
		return outcome{word: "completed"}
	}
	var exitErr *exec.ExitError
	if !errors.As(err, &exitErr) {
		return outcome{word: "failed", exitCode: 1, attrs: map[string]any{"error": err.Error()}}
	}
	ws, ok := exitErr.Sys().(syscall.WaitStatus)
	if ok && ws.Signaled() {
		name, known := signalNames[ws.Signal()]
		if !known {
			name = fmt.Sprintf("signal %d", int(ws.Signal()))
		}
		return outcome{word: "cancelled", exitCode: 128 + int(ws.Signal()),
			attrs: map[string]any{"signal": name}}
	}
	code := exitErr.ExitCode()
	return outcome{word: "failed", exitCode: code, attrs: map[string]any{"exit_code": code}}
}
