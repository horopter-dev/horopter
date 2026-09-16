package postgres

import (
	"errors"
	"time"

	"go.opentelemetry.io/otel/attribute"

	"github.com/yo61/horopter/internal/core"
)

// The dedupe paths an append can take. Distinguishing them matters because the second
// is the concurrent-retry recovery, which fires when two in-flight requests carry one
// idempotency key and the loser hits the unique index. It is exercised by a single
// test and is otherwise invisible; how often it happens in production says whether
// producer retry behaviour is what anyone assumed.
const (
	dedupeNone            = "none"
	dedupeLookup          = "lookup"
	dedupeUniqueViolation = "unique_violation"
)

// appendOutcome collects what one append did, for recording on its span in a single
// call. Accumulating and emitting once — rather than setting attributes from each
// branch — keeps instrumentation out of the control flow and produces one wide event
// per unit of work rather than a scatter of narrow ones.
type appendOutcome struct {
	created     bool
	dedupePath  string
	lockWait    time.Duration
	lockTimeout bool
	foldStatus  core.Status
	foldClosed  bool
	outOfOrder  bool
	errKind     string
}

// The error taxonomy, mirroring the core.ErrInvalidEvent sentinel that already decides
// 400 from 503 at the HTTP edge. The branch exists; this records it, so "how many
// appends were rejected as malformed" and "how many failed because the store was
// unavailable" become two different questions rather than one 5xx rate.
const (
	errKindInvalidEvent = "invalid_event"
	errKindTransient    = "transient"
)

// errKindOf classifies a failed append the same way appendFailed does at the HTTP edge.
// Both read the same sentinel, so a span and a status code can never disagree.
func errKindOf(err error) string {
	if err == nil {
		return ""
	}
	if errors.Is(err, core.ErrInvalidEvent) {
		return errKindInvalidEvent
	}
	return errKindTransient
}

// attributes renders the outcome and the event that produced it. High cardinality is
// deliberate: tenant, ref and event id are exactly the dimensions an incident is
// sliced by, and a backend that cannot hold them is the wrong backend.
func (o appendOutcome) attributes(ev core.Event) []attribute.KeyValue {
	attrs := o.baseAttributes(ev)
	if o.errKind != "" {
		// Present only on failure, so its absence is a reliable filter for success
		// rather than a value that has to be compared against "".
		attrs = append(attrs, attribute.String("horopter.error.kind", o.errKind))
	}
	return attrs
}

// The outcomes horopter.event.appends distinguishes. They are derived from created and
// errKind rather than stored, so the counter cannot disagree with the telemetry span, which
// records the same two facts.
const (
	outcomeCreated      = "created"
	outcomeDeduplicated = "deduplicated"
	outcomeFailed       = "failed"
)

// result names what the append did. A failure is checked first: an append can fail after
// its event was written, when the commit itself fails.
func (o appendOutcome) result() string {
	switch {
	case o.errKind != "":
		return outcomeFailed
	case o.created:
		return outcomeCreated
	default:
		return outcomeDeduplicated
	}
}

// metricAttributes is the bounded part of the outcome, for horopter.event.appends. Every
// value comes from a set fixed in code, and nothing identifies a caller or an entity: a
// metric keeps a series for every distinct set it has seen, and on the unauthenticated side
// a tenant is whatever string a caller sends. Tenant, ref and event id stay on the span.
//
// The keys match the span's, so a metric and a span describing the same append can be
// matched.
func (o appendOutcome) metricAttributes() attribute.Set {
	attrs := []attribute.KeyValue{
		attribute.String("horopter.append.outcome", o.result()),
		attribute.String("horopter.append.dedupe_path", o.dedupePath),
		attribute.Bool("horopter.fold.out_of_order", o.outOfOrder),
	}
	if o.errKind != "" {
		attrs = append(attrs, attribute.String("horopter.error.kind", o.errKind))
	}
	return attribute.NewSet(attrs...)
}

func (o appendOutcome) baseAttributes(ev core.Event) []attribute.KeyValue {
	return []attribute.KeyValue{
		// Repeated from the server span deliberately: a query that slices store spans by
		// tenant should not have to join across a trace to do it.
		attribute.String("horopter.tenant", ev.Tenant),
		attribute.String("horopter.event.id", ev.ID),
		attribute.String("horopter.event.type", ev.Type),
		attribute.String("horopter.event.source", ev.Source),
		attribute.String("horopter.event.subject", ev.Subject),
		attribute.String("horopter.event.ref", core.RefFor(ev)),
		attribute.Bool("horopter.event.correlated", ev.CorrelationRef != ""),
		attribute.Int64("horopter.event.clock_skew_ms",
			ev.ReceivedAt.Sub(ev.OccurredAt).Milliseconds()),
		attribute.Bool("horopter.append.created", o.created),
		attribute.String("horopter.append.dedupe_path", o.dedupePath),
		attribute.Int64("horopter.lock.wait_ms", o.lockWait.Milliseconds()),
		attribute.Bool("horopter.lock.timed_out", o.lockTimeout),
		attribute.String("horopter.fold.status", string(o.foldStatus)),
		attribute.Bool("horopter.fold.closed", o.foldClosed),
		attribute.Bool("horopter.fold.out_of_order", o.outOfOrder),
	}
}
