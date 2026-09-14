package core

import "strings"

// terminalSuffixes maps an event type's trailing segment to the status it
// implies. Any type whose last dot-separated segment appears here is terminal.
var terminalSuffixes = map[string]Status{
	"completed":   StatusCompleted,
	"finished":    StatusCompleted,
	"succeeded":   StatusCompleted,
	"failed":      StatusFailed,
	"rolled_back": StatusRolledBack,
	"rolledback":  StatusRolledBack,
	"cancelled":   StatusCancelled,
	"canceled":    StatusCancelled,
}

// StatusForType reports the terminal status implied by an event type, and
// whether the type is terminal at all.
func StatusForType(t string) (Status, bool) {
	idx := strings.LastIndex(t, ".")
	if idx < 0 || idx == len(t)-1 {
		return "", false
	}
	s, ok := terminalSuffixes[t[idx+1:]]
	return s, ok
}

func isStartType(t string) bool {
	return strings.HasSuffix(t, ".started") || strings.HasSuffix(t, ".begun")
}

// RefFor returns the projection key for an event: its correlation ref when it
// has one, otherwise its own id, which makes it a point.
func RefFor(ev Event) string {
	if ev.CorrelationRef != "" {
		return ev.CorrelationRef
	}
	return ev.ID
}

// Fold applies one event to a change and returns the result. cur is nil for
// the first event of a ref.
//
// Fold is pure: it reads no clock, consumes no randomness, and does not
// mutate cur or any map reachable from it. This is what makes a projection
// rebuild reproduce production exactly.
func Fold(cur *Change, ev Event) Change {
	var out Change
	if cur != nil {
		out = *cur
		out.Attributes = make(map[string]any, len(cur.Attributes)+len(ev.Attributes))
		for k, v := range cur.Attributes {
			out.Attributes[k] = v
		}
	} else {
		out = Change{
			Tenant:     ev.Tenant,
			Ref:        RefFor(ev),
			Subject:    ev.Subject,
			Start:      ev.OccurredAt, // fallback; a later .started overwrites it
			Attributes: make(map[string]any, len(ev.Attributes)),
		}
		if ev.CorrelationRef == "" {
			at := ev.OccurredAt
			out.End = &at
			out.Status = StatusOccurred
		}
	}

	for k, v := range ev.Attributes {
		out.Attributes[k] = v
	}

	out.Type = ev.Type
	out.Actor = ev.Actor
	out.LastEventID = ev.ID
	out.Title = ev.Title
	if out.Title == "" {
		out.Title = ev.Type
	}

	switch {
	case ev.CorrelationRef == "":
		// Point: start and end already pinned above.
	case isStartType(ev.Type):
		out.Start = ev.OccurredAt
		if out.End == nil {
			out.Status = StatusInProgress
		}
	default:
		if status, terminal := StatusForType(ev.Type); terminal {
			at := ev.OccurredAt
			out.End = &at
			out.Status = status
		}
	}

	return out
}
