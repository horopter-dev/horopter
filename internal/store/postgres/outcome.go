package postgres

import (
	"errors"
	"time"

	"go.opentelemetry.io/otel/attribute"

	"horopter.dev/horopter/internal/core"
)

// The second is concurrent-retry recovery, otherwise invisible in production.
const (
	dedupeNone            = "none"
	dedupeLookup          = "lookup"
	dedupeUniqueViolation = "unique_violation"
)

// Accumulated and emitted once, keeping instrumentation out of the control flow.
type appendOutcome struct {
	created     bool
	dedupePath  string
	lockWait    time.Duration
	lockTimeout bool
	foldStatus  core.Status
	foldClosed  bool
	outOfOrder  bool
	refoldMoved []string
	errKind     string
}

// Mirrors core.ErrInvalidEvent, so malformed and unavailable are counted apart.
const (
	errKindInvalidEvent = "invalid_event"
	errKindTransient    = "transient"
)

// Reads the same sentinel as appendFailed, so span and status never disagree.
func errKindOf(err error) string {
	if err == nil {
		return ""
	}
	if errors.Is(err, core.ErrInvalidEvent) {
		return errKindInvalidEvent
	}
	return errKindTransient
}

// High cardinality is deliberate: incidents are sliced by tenant, ref and event id.
func (o appendOutcome) attributes(ev core.Event) []attribute.KeyValue {
	attrs := o.baseAttributes(ev)
	if o.errKind != "" {
		// Present only on failure, so its absence filters for success.
		attrs = append(attrs, attribute.String("horopter.error.kind", o.errKind))
	}
	return attrs
}

// Derived from created and errKind, so the counter matches the span.
const (
	outcomeCreated      = "created"
	outcomeDeduplicated = "deduplicated"
	outcomeFailed       = "failed"
)

// Failure first: an append can fail after writing, when the commit fails.
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

// Bounded values only: a metric keeps a series per distinct set. Keys match the span's.
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
		// Repeated from the server span so store spans slice by tenant without a join.
		attribute.String("horopter.tenant", ev.Tenant),
		attribute.String("horopter.event.id", ev.ID),
		attribute.String("horopter.event.type", ev.Type),
		attribute.String("horopter.event.source", ev.Source),
		attribute.String("horopter.event.subject", ev.Subject),
		attribute.String("horopter.event.ref", core.RefFor(ev).String()),
		attribute.Bool("horopter.event.correlated", !ev.CorrelationRef.IsZero()),
		attribute.Int64("horopter.event.clock_skew_ms",
			ev.ReceivedAt.Sub(ev.OccurredAt).Milliseconds()),
		attribute.Bool("horopter.append.created", o.created),
		attribute.String("horopter.append.dedupe_path", o.dedupePath),
		attribute.Int64("horopter.lock.wait_ms", o.lockWait.Milliseconds()),
		attribute.Bool("horopter.lock.timed_out", o.lockTimeout),
		attribute.String("horopter.fold.status", string(o.foldStatus)),
		attribute.Bool("horopter.fold.closed", o.foldClosed),
		attribute.Bool("horopter.fold.out_of_order", o.outOfOrder),
		attribute.StringSlice("horopter.refold.moved", o.refoldMoved),
	}
}
