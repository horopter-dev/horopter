package core

import "time"

// Status is the lifecycle state of a change.
type Status string

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
// than Postgres 'infinity', which SQLite does not understand.
var OpenSentinel = time.Date(9999, 12, 31, 23, 59, 59, 0, time.UTC)

// Change is the derived, rebuildable projection that every render adapter
// reads. It takes one of three shapes: point, open, or closed.
type Change struct {
	Tenant      string
	Ref         string
	Subject     string
	Type        string
	Title       string
	Actor       string
	Start       time.Time
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
func (c Change) IsPoint() bool {
	return c.Status == StatusOccurred && c.End != nil && c.End.Equal(c.Start)
}

// EndEffective returns the end used for overlap comparisons, substituting
// OpenSentinel for an open change.
func (c Change) EndEffective() time.Time {
	if c.End == nil {
		return OpenSentinel
	}
	return *c.End
}
