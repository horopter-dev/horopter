package main

import (
	"context"
	"errors"
	"flag"
	"io"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"horopter.dev/horopter/internal/client"
)

// startWindow bounds the start post: the command waits on it. A variable so a test can shorten it.
var startWindow = 10 * time.Second

// abortWindow bounds the cancellation post after an interrupt: the user has asked to stop, so
// this is short. A variable so a test can shorten it.
var abortWindow = 3 * time.Second

// runner is one horo run invocation.
type runner struct {
	cl     *client.Client
	r      recording
	id     string // the invocation's ULID; idempotency keys are built from it, never the ref
	strict bool
	stderr io.Writer
}

func runRun(
	ctx context.Context, args []string, env func(string) string, stdout, stderr io.Writer,
) int {
	x, argv, ok := parseRun(args, env, stderr)
	if !ok {
		return exitUsage
	}
	cl, err := x.r.connect()
	if err != nil {
		return unrecorded(argv, err, x.strict, stdout, stderr)
	}
	x.cl = cl
	started := x.postStart(ctx)
	if ctx.Err() != nil {
		return x.abort(ctx)
	}
	if !started && x.strict {
		return exitError
	}
	out := runChild(argv, stdout, stderr, forwarded(sharesTerminal()))
	return x.finish(ctx, started, out)
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
	fs.BoolVar(&x.strict, "strict", false, "fail rather than run unrecorded, or succeed unrecorded")
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
func unrecorded(argv []string, why error, strict bool, stdout, stderr io.Writer) int {
	if strict {
		sayf(stderr, "horo run: %v; --strict, so the command was not run\n", why)
		return exitUsage
	}
	sayf(stderr, "horo run: %v; running the command unrecorded\n", why)
	return runChild(argv, stdout, stderr, forwarded(sharesTerminal())).exitCode
}

// event is an event about this invocation's change.
func (x *runner) event(word, phase string, at time.Time, attrs map[string]any) client.NewEvent {
	return client.NewEvent{
		Type: x.r.typ + "." + word, Subject: x.r.subject, Title: x.r.title, Actor: x.r.actor,
		CorrelationRef: x.r.ref, IdempotencyKey: "horo/" + x.id + "/" + phase, Lifecycle: word,
		OccurredAt: at, Attributes: attrs,
	}
}

func (x *runner) postStart(ctx context.Context) bool {
	got, err := x.cl.Post(ctx, x.event("started", "start", time.Now(), x.r.attributes()),
		time.Now().Add(startWindow))
	if err != nil {
		sayf(x.stderr, "horo: could not record the start (%v); ref %s (--ref)\n",
			explain(err, producerHint), x.r.ref)
		return false
	}
	sayf(x.stderr, "horo: recording %s (horo show) — ref %s (--ref)\n", got.Ref, x.r.ref)
	return true
}

// abort handles an interrupt during the start post: the command is not run. The start may
// already have reached ingress, so the change is closed as cancelled rather than left open.
func (x *runner) abort(ctx context.Context) int {
	at := time.Now()
	attrs := map[string]any{"reason": "interrupted before the command started"}
	err := x.postUnlessInterrupted(ctx, x.event("cancelled", "end", at, attrs), abortWindow)
	if err != nil {
		sayf(x.stderr, "horo run: interrupted before the command started; it was not run, and the "+
			"cancellation could not be confirmed (%v). If the change is still open, close it "+
			"with:\n  %s\n",
			explain(err, producerHint), x.closeCommand("cancelled", at))
	} else {
		say(x.stderr, "horo run: interrupted before the command started; it was not run, "+
			"and is recorded as cancelled")
	}
	return signalExit(ctx)
}

// finish records how the command ended and picks horo's exit code.
func (x *runner) finish(ctx context.Context, started bool, out outcome) int {
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
	err := x.postUnlessInterrupted(ctx, x.event(out.word, "end", at, attrs), endWindow)
	if err == nil {
		return out.exitCode
	}
	sayf(x.stderr, "horo: could not confirm the end was recorded (%v). If the change is still "+
		"open, close it with:\n  %s\n", explain(err, producerHint), x.closeCommand(out.word, at))
	if x.strict {
		return exitError
	}
	return out.exitCode
}

// postUnlessInterrupted posts ev, giving up if a signal arrives first. It is used once the
// command has ended, or was never started: whoever interrupts then is waiting only on Horopter,
// and wants the close command rather than more waiting. An interrupt cancels run's context, so
// the post runs without it.
func (x *runner) postUnlessInterrupted(
	ctx context.Context, ev client.NewEvent, window time.Duration,
) error {
	again := make(chan os.Signal, 1)
	signal.Notify(again, os.Interrupt, syscall.SIGTERM)
	defer signal.Stop(again)
	posted := make(chan error, 1)
	go func() {
		_, err := x.cl.Post(context.WithoutCancel(ctx), ev, time.Now().Add(window))
		posted <- err
	}()
	select {
	case err := <-posted:
		return err
	case <-again:
		return errors.New("interrupted while waiting for Horopter")
	}
}

// closeCommand is the horo event that closes the change, every argument single-quoted so a
// subject with spaces or quotes pastes as one word. It uses the producer ref: --ref takes that
// form, and the stored <locus>/<ref> would have its locus doubled. It carries the time the
// command ended, so a change closed hours later does not count the outage in its duration, and
// the end post's own idempotency key: a post that timed out may still have been stored, and
// then pasting this is a harmless replay rather than a second end.
func (x *runner) closeCommand(word string, at time.Time) string {
	args := []string{"horo", "event", "--ref", x.r.ref, "--subject", x.r.subject,
		"--type", x.r.typ + "." + word, "--lifecycle", word,
		"--occurred-at", at.UTC().Format(time.RFC3339Nano),
		"--idempotency-key", "horo/" + x.id + "/end"}
	quoted := make([]string, len(args))
	for i, a := range args {
		quoted[i] = shellQuote(a)
	}
	return strings.Join(quoted, " ")
}

func shellQuote(s string) string {
	if s != "" && !strings.ContainsAny(s, " '\"\\$`!*?&;|<>(){}[]#~") {
		return s
	}
	// A single quote is written '"'"': close the quote, a double-quoted ', reopen.
	return "'" + strings.ReplaceAll(s, "'", `'"'"'`) + "'"
}
