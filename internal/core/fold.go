package core

import "strings"

// StatusForType reports the terminal status implied by an event type, and
// whether the type is terminal at all. The terminal vocabulary is the event
// type's last dot-separated segment; a switch (rather than a package-level
// map) keeps it fixed at compile time so nothing in the package can rewrite
// it at runtime.
func StatusForType(t string) (Status, bool) {
	idx := strings.LastIndex(t, ".")
	if idx < 0 || idx == len(t)-1 {
		return "", false
	}
	switch t[idx+1:] {
	case "completed", "finished", "succeeded", "resolved", "closed", "done", "ended":
		return StatusCompleted, true
	case "failed", "errored", "timed_out":
		return StatusFailed, true
	case "rolled_back", "rolledback":
		return StatusRolledBack, true
	case "cancelled", "canceled", "aborted":
		return StatusCancelled, true
	default:
		return "", false
	}
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
// the first event of a ref, but a zero-value *Change (as produced by, e.g.,
// `c := m[ref]; m[ref] = Fold(&c, ev)` over a map[string]Change) must yield
// the identical result — Task 8 rebuilds the whole projection with exactly
// that idiom.
//
// Fold is pure: it reads no clock, consumes no randomness, and does not
// mutate cur or any map reachable from it. This is what makes a projection
// rebuild reproduce production exactly. The attribute copy is shallow in its
// values, though: a nested map[string]any or []any value is shared with the
// input event rather than cloned. Fold never mutates such values, so purity
// still holds, but the returned Change is not deeply independent of ev.
func Fold(cur *Change, ev Event) Change {
	var out Change
	if cur != nil {
		out = *cur
		out.Attributes = make(map[string]any, len(cur.Attributes)+len(ev.Attributes))
		for k, v := range cur.Attributes {
			out.Attributes[k] = v
		}
	} else {
		out = Change{Attributes: make(map[string]any, len(ev.Attributes))}
	}

	for k, v := range ev.Attributes {
		out.Attributes[k] = v
	}

	out.Tenant = ev.Tenant
	out.Ref = RefFor(ev)
	out.Subject = ev.Subject
	out.Type = ev.Type
	out.Actor = ev.Actor
	out.LastEventID = ev.ID
	out.Title = ev.Title
	if out.Title == "" {
		out.Title = ev.Type
	}
	if out.Start.IsZero() {
		out.Start = ev.OccurredAt // fallback; a later .started overwrites it
	}

	switch {
	case ev.CorrelationRef == "":
		at := ev.OccurredAt
		out.End = &at
		out.Status = StatusOccurred
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
