package main

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"time"

	"horopter.dev/horopter/internal/client"
	"horopter.dev/horopter/internal/core"
)

const producerHint = "--key / HOROPTER_INGRESS_KEY must be a producer key"

// endWindow bounds an event's post, and run's end post. A variable so a test can shorten it.
var endWindow = 30 * time.Second

// recording are the flags run and event share.
type recording struct {
	url, key, typ, subject, title, actor, ref string
	attrs                                     attrs
}

func (r *recording) register(fs *flag.FlagSet, env func(string) string) {
	fs.StringVar(&r.url, "ingress-url", env("HOROPTER_INGRESS_URL"), "ingress base URL")
	r.key = env("HOROPTER_INGRESS_KEY")
	fs.Var(secret{&r.key}, "key", "a producer key (default $HOROPTER_INGRESS_KEY)")
	fs.StringVar(&r.typ, "type", "", "event type, e.g. deployment.started")
	fs.StringVar(&r.subject, "subject", "", "what the change happened to, e.g. acme/prod")
	fs.StringVar(&r.title, "title", "", "a human-readable title")
	fs.StringVar(&r.actor, "actor", "", "who or what performed it")
	fs.StringVar(&r.ref, "ref", "", "the change's ref; without one, an event is a point")
	r.attrs = attrs{}
	fs.Var(r.attrs, "attr", "key=value attribute (repeatable)")
}

// check reports a usage error in what the event says.
func (r *recording) check() error {
	switch {
	case r.typ == "":
		return errors.New("--type is required")
	case r.subject == "":
		return errors.New("--subject is required")
	}
	return nil
}

// target reports a missing destination, naming this side's flags and variables. horo event
// treats it as a usage error; horo run as Horopter being unreachable.
func (r *recording) target() error {
	switch {
	case r.url == "":
		return errors.New("no ingress URL: set --ingress-url or HOROPTER_INGRESS_URL")
	case r.key == "":
		return errors.New("no key: set --key or HOROPTER_INGRESS_KEY to a producer key")
	}
	return nil
}

func (r *recording) client() (*client.Client, error) {
	return client.New(r.url, r.key)
}

func (r *recording) attributes() map[string]any {
	out := make(map[string]any, len(r.attrs))
	for k, v := range r.attrs {
		out[k] = v
	}
	return out
}

// newInvocationID identifies one horo invocation. Idempotency keys are built from it, not from
// the ref: ingress's uniqueness is (tenant, idempotency_key) with no locus, so a key built from
// a ref two producers both chose would make the second one's event a duplicate.
func newInvocationID() string {
	return core.NewULID(time.Now(), rand.Reader)
}

func runEvent(
	ctx context.Context, args []string, env func(string) string, stdout, stderr io.Writer,
) int {
	fs := flag.NewFlagSet("event", flag.ContinueOnError)
	fs.SetOutput(stderr)
	var r recording
	r.register(fs, env)
	lifecycle := fs.String("lifecycle", "",
		"started, completed, failed, rolled_back, cancelled or none")
	occurred := fs.String("occurred-at", "", "when it happened, RFC 3339; default now")
	key := fs.String("idempotency-key", "", "deduplicates a retried post; default per invocation")
	if err := fs.Parse(args); err != nil {
		return exitUsage
	}
	at, err := occurredAt(*occurred)
	if err != nil {
		say(stderr, "horo event: --occurred-at:", err)
		return exitUsage
	}
	if err := errors.Join(r.check(), r.target()); err != nil {
		say(stderr, "horo event:", err)
		return exitUsage
	}
	if *key == "" {
		*key = "horo/" + newInvocationID()
	}
	cl, err := r.client()
	if err != nil {
		say(stderr, "horo event:", err)
		return exitUsage
	}
	got, err := cl.Post(ctx, client.NewEvent{
		Type: r.typ, Subject: r.subject, Title: r.title, Actor: r.actor, CorrelationRef: r.ref,
		IdempotencyKey: *key, Lifecycle: *lifecycle, OccurredAt: at, Attributes: r.attributes(),
	}, time.Now().Add(endWindow))
	if err != nil {
		return report(stderr, "horo event", explain(err, producerHint))
	}
	if err := json.NewEncoder(stdout).Encode(got); err != nil {
		return report(stderr, "horo event", fmt.Errorf("write the answer: %w", err))
	}
	return exitOK
}

// occurredAt is --occurred-at's time: now when it is not given.
func occurredAt(s string) (time.Time, error) {
	if s == "" {
		return time.Now(), nil
	}
	return time.Parse(time.RFC3339, s)
}
