package core

import "time"

// Status is the lifecycle state of a change.
type Status string

// The Status values are the lifecycle a change can pass through. StatusOccurred marks
// an instantaneous change with no correlation ref (a point, never an interval).
// StatusInProgress, StatusCompleted, StatusFailed, StatusRolledBack and StatusCancelled
// are the possible outcomes of a correlated change that has a start and, once one of
// the terminal statuses is folded in, an end.
const (
	StatusOccurred   Status = "occurred"
	StatusInProgress Status = "in_progress"
	StatusCompleted  Status = "completed"
	StatusFailed     Status = "failed"
	StatusRolledBack Status = "rolled_back"
	StatusCancelled  Status = "cancelled"
)

// OpenSentinel stands in for the end of an open change so that the overlap
// predicate can be served by a plain index. It is a far-future literal rather
// than Postgres 'infinity' because Go's time.Time cannot represent infinity.
var OpenSentinel = time.Date(9999, 12, 31, 23, 59, 59, 0, time.UTC)

// Change is the derived, rebuildable projection that every render adapter
// reads. It takes one of three shapes: point, open, or closed.
//
// A change's start is two facts rather than one. StartedAt is the ASSERTION —
// a start-type event's own occurred_at — and is nil until such an event
// arrives. FirstSeenAt is the INFERENCE: the earliest occurred_at of any event
// folded onto the ref, evidence that the change must have existed by then.
// Start is COALESCE(StartedAt, FirstSeenAt) and mirrors the generated start_at
// column, so consumers that only want "when did this start" read Start and are
// unaffected by the split. FirstSeenAt < StartedAt is a real and queryable
// anomaly — an event claiming to predate the change's declared start — rather
// than something the fold has erased. See
// decisions/2026-09-15-change-start-time-provenance.md.
type Change struct {
	Tenant      string
	Ref         string
	Subject     string
	Type        string
	Title       string
	Actor       string
	Start       time.Time
	StartedAt   *time.Time
	FirstSeenAt time.Time
	End         *time.Time
	Status      Status
	Attributes  map[string]any
	LastEventID string
}

// IsOpen reports whether the change is still in flight. An open change renders
// as a region growing to the present.
func (c Change) IsOpen() bool { return c.End == nil }

// IsPoint reports whether the change is instantaneous. A point renders as a
// line, never as a region. Timestamp equality alone cannot distinguish a
// point from a closed interval that happens to have zero duration (a
// same-second start/complete pair, common in backfills and idempotent
// producers), so IsPoint also requires StatusOccurred, which only the
// no-correlation-ref path ever sets.
//
// Unlike IsOpen it has no production caller, and that is deliberate rather
// than an oversight waiting to be tidied away. Neither render adapter needs
// to branch on point-ness: a point's End equals its Start, so it already
// renders as a zero-duration span and a Grafana annotation whose timeEnd
// equals its time, without either adapter asking. What IsPoint carries is the
// rule above — that the third of spec §5.3's three shapes is identified by
// status and not by timestamp equality — which nothing else in this package
// states and which core.Fold's tests assert through this method. Deleting it
// would delete the rule, not just an unused function.
func (c Change) IsPoint() bool {
	return c.Status == StatusOccurred && c.End != nil && c.End.Equal(c.Start)
}
