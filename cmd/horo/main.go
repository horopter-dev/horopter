// Command horo explores what Horopter recorded: the changes a window and filters select, and
// one change with all its events. It reads egress with a reader key.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"horopter.dev/horopter/internal/client"
)

const (
	exitOK       = 0
	exitError    = 1
	exitUsage    = 2
	exitNotFound = 3
)

func main() {
	info, err := os.Stdout.Stat()
	tty := err == nil && info.Mode()&os.ModeCharDevice != 0
	os.Exit(run(os.Args[1:], os.Getenv, os.Stdout, os.Stderr, tty))
}

func run(args []string, env func(string) string, stdout, stderr io.Writer, tty bool) int {
	if len(args) == 0 {
		usage(stderr)
		return exitUsage
	}
	ctx, stop := interruptible()
	defer stop()
	switch args[0] {
	case "changes":
		return runChanges(ctx, args[1:], env, stdout, stderr, tty)
	case "show":
		return runShow(ctx, args[1:], env, stdout, stderr, tty)
	case "event":
		return runEvent(ctx, args[1:], env, stdout, stderr)
	case "run":
		return runRun(ctx, args[1:], env, stdout, stderr)
	case "refold":
		return runRefold(ctx, args[1:], env, stdout, stderr)
	default:
		sayf(stderr, "horo: unknown command %q\n", args[0])
		usage(stderr)
		return exitUsage
	}
}

// interruptible is a context that ends when SIGINT, SIGTERM, SIGHUP or SIGQUIT arrives, with the
// signal as its cause: a dropped SSH session or Ctrl-\ must not kill horo and leave the change
// open. horo keeps running, so it can still record how a wrapped command ended.
func interruptible() (context.Context, func()) {
	ctx, cancel := context.WithCancelCause(context.Background())
	signals := make(chan os.Signal, 1)
	notify(signals, os.Interrupt, syscall.SIGTERM, syscall.SIGHUP, syscall.SIGQUIT)
	go func() {
		select {
		case s := <-signals:
			if sig, ok := s.(syscall.Signal); ok {
				cancel(interrupted{sig})
			}
		case <-ctx.Done():
		}
	}()
	return ctx, func() {
		signal.Stop(signals)
		cancel(nil)
	}
}

// interrupted is the cause of run's context ending on a signal.
type interrupted struct{ sig syscall.Signal }

func (i interrupted) Error() string { return "interrupted by " + i.sig.String() }

// signalExit is the shell's exit code for the signal that ended ctx.
func signalExit(ctx context.Context) int {
	var i interrupted
	if errors.As(context.Cause(ctx), &i) {
		return 128 + int(i.sig)
	}
	return 128 + int(syscall.SIGINT)
}

func usage(w io.Writer) {
	say(w, "usage: horo changes [flags]           list changes in a window\n"+
		"       horo show [flags] <ref>         one change and its events\n"+
		"       horo event [flags]              record one event\n"+
		"       horo run [flags] -- <command>   record a command's start and outcome\n"+
		"       horo refold [flags] <ref>       re-fold one change from its log")
}

// common are the flags every command takes.
type common struct {
	url, key, output string
	utc              bool
}

func (c *common) register(fs *flag.FlagSet, env func(string) string, tty bool) {
	fs.StringVar(&c.url, "egress-url", env("HOROPTER_EGRESS_URL"), "egress base URL")
	c.key = env("HOROPTER_EGRESS_KEY")
	fs.Var(secret{&c.key}, "key", "a reader key (default $HOROPTER_EGRESS_KEY)")
	def := "jsonl"
	if tty {
		def = "table"
	}
	fs.StringVar(&c.output, "output", def, "table, json or jsonl")
	fs.BoolVar(&c.utc, "utc", false, "show times in UTC")
}

func (c *common) client() (*client.Client, error) {
	if c.output != "table" && c.output != "json" && c.output != "jsonl" {
		return nil, fmt.Errorf("--output %q: use table, json or jsonl", c.output)
	}
	if c.url == "" {
		return nil, errors.New("no egress URL: set --egress-url or HOROPTER_EGRESS_URL")
	}
	if c.key == "" {
		return nil, errors.New("no key: set --key or HOROPTER_EGRESS_KEY to a reader key")
	}
	return client.New(c.url, c.key)
}

const readerHint = "--key / HOROPTER_EGRESS_KEY must be a reader key"

// secret is a flag whose default comes from the environment but is never shown: usage output
// prints a flag's default, and CI keeps stderr in its logs.
type secret struct{ v *string }

func (s secret) String() string     { return "" }
func (s secret) Set(v string) error { *s.v = v; return nil }

// explain adds which key a refused request needed: the client cannot know which side it serves.
func explain(err error, keyHint string) error {
	var he *client.HTTPError
	if errors.As(err, &he) && he.Status == http.StatusUnauthorized {
		return fmt.Errorf("%w; %s", err, keyHint)
	}
	return err
}

// attrs collects repeated --attr key=value flags.
type attrs map[string]string

func (a attrs) String() string { return "" }

func (a attrs) Set(v string) error {
	k, val, ok := strings.Cut(v, "=")
	if !ok || k == "" {
		return fmt.Errorf("--attr %q: use key=value", v)
	}
	a[k] = val
	return nil
}

func runChanges(
	ctx context.Context, args []string, env func(string) string, stdout, stderr io.Writer, tty bool,
) int {
	fs := flag.NewFlagSet("changes", flag.ContinueOnError)
	fs.SetOutput(stderr)
	var c common
	c.register(fs, env, tty)
	var q client.ChangeQuery
	since := fs.Duration("since", 24*time.Hour, "how far back from now")
	from := fs.String("from", "", "window start, RFC 3339")
	to := fs.String("to", "", "window end, RFC 3339")
	fs.StringVar(&q.Subject, "subject", "", "exact subject")
	fs.StringVar(&q.Type, "type", "", "exact type")
	fs.StringVar(&q.Status, "status", "", "exact status")
	fs.StringVar(&q.Locus, "locus", "", "exact locus")
	fs.StringVar(&q.Source, "source", "", "a contributing source")
	q.Attrs = attrs{}
	fs.Var(attrs(q.Attrs), "attr", "key=value on an allowlisted attribute (repeatable)")
	fs.IntVar(&q.Limit, "limit", 0, "at most this many changes; 0 for all")
	if err := fs.Parse(args); err != nil {
		return exitUsage
	}
	if err := window(fs, &q, *since, *from, *to); err != nil {
		say(stderr, "horo changes:", err)
		return exitUsage
	}
	cl, err := c.client()
	if err != nil {
		say(stderr, "horo changes:", err)
		return exitUsage
	}
	return report(stderr, "horo changes", explain(
		writeChanges(stdout, c.output, location(c.utc), cl.Changes(ctx, q)), readerHint))
}

// window sets q's window from --since, or from --from and --to; giving both is an error rather
// than a silent choice of one.
func window(fs *flag.FlagSet, q *client.ChangeQuery, since time.Duration, from, to string) error {
	sinceSet := false
	fs.Visit(func(f *flag.Flag) { sinceSet = sinceSet || f.Name == "since" })
	if from == "" && to == "" {
		q.To = time.Now()
		q.From = q.To.Add(-since)
		return nil
	}
	if sinceSet {
		return errors.New("give --since or --from/--to, not both")
	}
	var err error
	q.From, q.To, err = explicitWindow(from, to)
	return err
}

// explicitWindow parses --from and --to; --to defaults to now.
func explicitWindow(from, to string) (time.Time, time.Time, error) {
	if from == "" {
		return time.Time{}, time.Time{}, errors.New("--to needs --from")
	}
	start, err := time.Parse(time.RFC3339, from)
	if err != nil {
		return time.Time{}, time.Time{}, fmt.Errorf("--from %q: %w", from, err)
	}
	end := time.Now()
	if to != "" {
		if end, err = time.Parse(time.RFC3339, to); err != nil {
			return time.Time{}, time.Time{}, fmt.Errorf("--to %q: %w", to, err)
		}
	}
	return start, end, nil
}

func runShow(
	ctx context.Context, args []string, env func(string) string, stdout, stderr io.Writer, tty bool,
) int {
	fs := flag.NewFlagSet("show", flag.ContinueOnError)
	fs.SetOutput(stderr)
	var c common
	c.register(fs, env, tty)
	if err := fs.Parse(args); err != nil {
		return exitUsage
	}
	if fs.NArg() != 1 {
		say(stderr, "horo show: give exactly one ref, e.g. ci/dep-1")
		return exitUsage
	}
	ref := fs.Arg(0)
	cl, err := c.client()
	if err != nil {
		say(stderr, "horo show:", err)
		return exitUsage
	}
	ch, found, err := cl.Change(ctx, ref)
	if err != nil {
		return report(stderr, "horo show", explain(err, readerHint))
	}
	if !found {
		sayf(stderr, "horo show: no change %q\n", ref)
		return exitNotFound
	}
	return report(stderr, "horo show", explain(
		writeShow(stdout, c.output, location(c.utc), ch, cl.Events(ctx, ref)), readerHint))
}

func location(utc bool) *time.Location {
	if utc {
		return time.UTC
	}
	return time.Local
}

// say and sayf write a diagnostic. A failed write to stderr has nowhere left to be reported.
func say(w io.Writer, a ...any) { _, _ = fmt.Fprintln(w, a...) }

func sayf(w io.Writer, format string, a ...any) { _, _ = fmt.Fprintf(w, format, a...) }

func report(stderr io.Writer, cmd string, err error) int {
	if err == nil {
		return exitOK
	}
	sayf(stderr, "%s: %v\n", cmd, err)
	return exitError
}
