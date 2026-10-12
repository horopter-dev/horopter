package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"horopter.dev/horopter/internal/client"
	"horopter.dev/horopter/internal/core"
)

// startWindow bounds the start post: the command waits on it. A variable so a test can shorten it.
var startWindow = 10 * time.Second

// abortWindow bounds the cancellation post when the command is not run: after an interrupt the
// user has asked to stop, and under --strict the start has already used its window, so this is
// short. A variable so a test can shorten it.
var abortWindow = 3 * time.Second

// runner is one horo run invocation.
type runner struct {
	cl         *client.Client
	r          recording
	id         string // the invocation's ULID; idempotency keys are built from it, never the ref
	strict     bool
	stderr     io.Writer
	reportPath string // where --report writes report; "" for nowhere
	report     runReport
}

func runRun(
	ctx context.Context, args []string, env func(string) string, stdout, stderr io.Writer,
) int {
	x, argv, ok := parseRun(args, env, stderr)
	if !ok {
		return exitUsage
	}
	x.report.ProducerRef = x.r.ref
	x.report.ExitCode = x.recordRun(ctx, argv, stdout)
	x.report.write(x.reportPath, stderr)
	return x.report.ExitCode
}

// recordRun runs argv, recording its start and outcome, and returns horo's exit code.
func (x *runner) recordRun(ctx context.Context, argv []string, stdout io.Writer) int {
	cl, err := x.r.connect()
	if err != nil {
		return x.unrecorded(argv, err, stdout)
	}
	x.cl = cl
	startErr := x.postStart(ctx)
	if ctx.Err() != nil {
		return x.abort(ctx)
	}
	if startErr != nil && x.strict {
		return x.refuse(ctx, startErr)
	}
	out := runChild(argv, stdout, x.stderr, forwarded(sharesTerminal()))
	return x.finish(ctx, startErr == nil, out)
}

// parseRun reads run's flags and the command after --. ok is false after a usage error, which
// it has already reported.
func parseRun(
	args []string, env func(string) string, stderr io.Writer,
) (x *runner, argv []string, ok bool) {
	fs := flag.NewFlagSet("run", flag.ContinueOnError)
	fs.SetOutput(stderr)
	x = &runner{stderr: stderr, id: newInvocationID()}
	x.r.register(fs, env)
	fs.Lookup("type").Usage = "the change's type, e.g. deployment; run adds .started and the outcome"
	fs.BoolVar(&x.strict, "strict", false, "fail rather than run unrecorded, or succeed unrecorded")
	fs.StringVar(&x.reportPath, "report", env("HORO_RUN_REPORT"),
		"write a JSON report of what was recorded to this file")
	if err := fs.Parse(args); err != nil {
		return nil, nil, false
	}
	argv = fs.Args()
	if !afterDashes(args, argv) {
		say(stderr, "horo run: give the command after --, "+
			"e.g. horo run --type deploy --subject a -- ./deploy.sh")
		return nil, nil, false
	}
	if err := x.r.check(); err != nil {
		say(stderr, "horo run:", err)
		return nil, nil, false
	}
	if x.r.ref == "" {
		x.r.ref = newInvocationID()
	}
	return x, argv, true
}

// afterDashes reports whether argv, what flag left unparsed, followed a --. flag also stops at
// the first non-flag argument, and then a flag written after the command would reach the command.
func afterDashes(args, argv []string) bool {
	dash := len(args) - len(argv) - 1
	return len(argv) > 0 && dash >= 0 && args[dash] == "--"
}

// connect is the client run posts with. A missing URL or key is Horopter being unreachable as
// far as run is concerned: an unset CI secret expands to "", and must not fail the deploy.
func (r *recording) connect() (*client.Client, error) {
	if err := r.target(); err != nil {
		return nil, err
	}
	return r.client()
}

// unrecorded runs the command without recording it, or under --strict refuses to.
func (x *runner) unrecorded(argv []string, why error, stdout io.Writer) int {
	x.report.because(why)
	if x.strict {
		sayf(x.stderr, "horo run: %v; --strict, so the command was not run\n", why)
		return exitError
	}
	sayf(x.stderr, "horo run: %v; running the command unrecorded\n", why)
	out := runChild(argv, stdout, x.stderr, forwarded(sharesTerminal()))
	x.report.ended(out.lifecycle)
	return out.exitCode
}

// eventType is the type of this invocation's event at lc. The close command must carry the type
// the end post would have, so both build it here.
func (x *runner) eventType(lc core.Lifecycle) string { return x.r.typ + "." + string(lc) }

// event is an event about this invocation's change.
func (x *runner) event(
	lc core.Lifecycle, phase string, at time.Time, attrs map[string]any,
) client.NewEvent {
	return client.NewEvent{
		Type: x.eventType(lc), Subject: x.r.subject, Title: x.r.title, Actor: x.r.actor,
		CorrelationRef: x.r.ref, IdempotencyKey: "horo/" + x.id + "/" + phase, Lifecycle: string(lc),
		OccurredAt: at, Attributes: attrs,
	}
}

// postStart posts the start, and returns why it could not be recorded.
func (x *runner) postStart(ctx context.Context) error {
	got, err := x.cl.Post(ctx, x.event(core.LifecycleStarted, "start", time.Now(), x.r.attributes()),
		time.Now().Add(startWindow))
	if err != nil {
		err = explain(err, producerHint)
		sayf(x.stderr, "horo: could not record the start (%v); ref %s (--ref)\n", err, x.r.ref)
		x.report.because(fmt.Errorf("could not record the start: %w", err))
		return err
	}
	sayf(x.stderr, "horo: recording %s (horo show) — ref %s (--ref)\n", got.Ref, x.r.ref)
	x.report.started(got.Ref)
	return nil
}

// abort handles an interrupt during the start post: the command is not run.
func (x *runner) abort(ctx context.Context) int {
	why := "interrupted before the command started"
	x.cancel(ctx, why, errors.New(why))
	return signalExit(ctx)
}

// refuse handles --strict after the start could not be recorded: the command is not run. Unless
// ingress refused the start, it may have stored it, perhaps on an attempt before the last.
func (x *runner) refuse(ctx context.Context, startErr error) int {
	if client.Refused(startErr) {
		say(x.stderr, "horo run: --strict, so the command was not run")
		return exitError
	}
	x.cancel(ctx, "--strict, and the start could not be recorded",
		fmt.Errorf("could not record the start: %w", startErr))
	return exitError
}

// cancel closes the change as cancelled when the command was not run: the start may already
// have reached ingress, and the change must not be left open. why is the event's reason and
// begins the warning; reason is the report's, extended if the cancellation is not confirmed.
func (x *runner) cancel(ctx context.Context, why string, reason error) {
	at := time.Now()
	x.report.ended(core.LifecycleCancelled)
	err := x.postEnd(ctx, x.event(core.LifecycleCancelled, "end", at, map[string]any{"reason": why}),
		abortWindow)
	if err != nil {
		err = explain(err, producerHint)
		sayf(x.stderr, "horo run: %s; the command was not run, and the cancellation could not be "+
			"confirmed (%v). If the change is still open, close it with:\n  %s\n",
			why, err, x.closeCommand(core.LifecycleCancelled, at))
		x.report.because(fmt.Errorf("%w; the cancellation could not be confirmed: %w", reason, err))
		return
	}
	say(x.stderr, "horo run: "+why+"; the command was not run, and is recorded as cancelled")
	x.report.because(reason)
}

// finish records how the command ended and picks horo's exit code.
func (x *runner) finish(ctx context.Context, started bool, out outcome) int {
	x.report.ended(out.lifecycle)
	if !started {
		// An end without its start would record a change that never began, and would hold the
		// step for endWindow against a Horopter already known to be unreachable.
		say(x.stderr, "horo run: the start was not recorded, "+
			"so the outcome is not recorded either")
		return out.exitCode
	}
	attrs := x.r.attributes()
	for k, v := range out.attrs {
		attrs[k] = v
	}
	at := time.Now()
	err := x.postEnd(ctx, x.event(out.lifecycle, "end", at, attrs), endWindow)
	if err == nil {
		return out.exitCode
	}
	err = explain(err, producerHint)
	sayf(x.stderr, "horo: could not confirm the end was recorded (%v). If the change is still "+
		"open, close it with:\n  %s\n", err, x.closeCommand(out.lifecycle, at))
	x.report.because(fmt.Errorf("could not confirm the end was recorded: %w", err))
	if x.strict {
		return exitError
	}
	return out.exitCode
}

// postEnd posts the end event ev, and records in the report that it was confirmed.
func (x *runner) postEnd(ctx context.Context, ev client.NewEvent, window time.Duration) error {
	got, err := x.postUnlessInterrupted(ctx, ev, window)
	if err == nil {
		x.report.endPosted(got.Ref)
	}
	return err
}

// postUnlessInterrupted posts ev, giving up if a signal arrives first. It is used once the
// command has ended, or was never started: whoever interrupts then is waiting only on Horopter,
// and wants the close command rather than more waiting. An interrupt cancels run's context, so
// the post runs without it.
func (x *runner) postUnlessInterrupted(
	ctx context.Context, ev client.NewEvent, window time.Duration,
) (client.Appended, error) {
	again := make(chan os.Signal, 1)
	notify(again, os.Interrupt, syscall.SIGTERM)
	defer signal.Stop(again)
	type answer struct {
		got client.Appended
		err error
	}
	posted := make(chan answer, 1)
	go func() {
		got, err := x.cl.Post(context.WithoutCancel(ctx), ev, time.Now().Add(window))
		posted <- answer{got, err}
	}()
	select {
	case a := <-posted:
		return a.got, a.err
	case <-again:
		return client.Appended{}, errors.New("interrupted while waiting for Horopter")
	}
}

// closeCommand is the horo event that closes the change, every argument single-quoted so a
// subject with spaces or quotes pastes as one word. It uses the producer ref: --ref takes that
// form, and the stored <locus>/<ref> would have its locus doubled. It carries the time the
// command ended, so a change closed hours later does not count the outage in its duration, and
// the end post's own idempotency key: a post that timed out may still have been stored, and
// then pasting this is a harmless replay rather than a second end.
func (x *runner) closeCommand(lc core.Lifecycle, at time.Time) string {
	args := []string{"horo", "event", "--ref", x.r.ref, "--subject", x.r.subject,
		"--type", x.eventType(lc), "--lifecycle", string(lc),
		"--occurred-at", at.UTC().Format(time.RFC3339Nano),
		"--idempotency-key", "horo/" + x.id + "/end"}
	quoted := make([]string, len(args))
	for i, a := range args {
		quoted[i] = shellQuote(a)
	}
	return strings.Join(quoted, " ")
}

// shellSafe are the characters no shell treats specially, so a word made only of them needs no
// quotes.
const shellSafe = "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789_./:@%+,=-"

// shellQuote quotes s unless every character is shell-safe — an allowlist, so a tab or the
// trailing newline a CI variable carries is quoted too. A leading = is quoted: zsh expands =cmd.
func shellQuote(s string) string {
	unsafe := func(r rune) bool { return !strings.ContainsRune(shellSafe, r) }
	if s != "" && !strings.HasPrefix(s, "=") && !strings.ContainsFunc(s, unsafe) {
		return s
	}
	// A single quote is written '"'"': close the quote, a double-quoted ', reopen.
	return "'" + strings.ReplaceAll(s, "'", `'"'"'`) + "'"
}
