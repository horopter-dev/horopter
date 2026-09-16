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

// mergedAttributes returns cur's attributes overlaid with ev's, in a fresh map. Fold's
// purity depends on that copy: the returned Change must never share an attribute map with
// cur, or folding one event would mutate a Change the caller still holds.
func mergedAttributes(cur *Change, ev Event) map[string]any {
	out := make(map[string]any, len(ev.Attributes))
	if cur != nil {
		out = make(map[string]any, len(cur.Attributes)+len(ev.Attributes))
		for k, v := range cur.Attributes {
			out[k] = v
		}
	}
	for k, v := range ev.Attributes {
		out[k] = v
	}
	return out
}

// Fold applies one event to a change and returns the result. cur is nil for
// the first event of a ref, but a zero-value *Change (as produced by, e.g.,
// `c := m[ref]; m[ref] = Fold(&c, ev)` over a map[string]Change) must yield
// the identical result. Both spellings are natural when accumulating into a
// map, and a rebuild that picked the zero-value one must not diverge from the
// incremental path, which passes nil.
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
	}
	out.Attributes = mergedAttributes(cur, ev)

	out.Tenant = ev.Tenant
	out.Ref = RefFor(ev)
	out.Subject = ev.Subject
	out.Type = ev.Type
	// Last NON-EMPTY wins. An unconditional assignment destroys information here,
	// because eventFromCloudEvent never sets Actor: any CloudEvent folded onto a ref
	// blanked the actor a previous event had recorded, and a rebuild reproduced the
	// erasure rather than repairing it. Subject and Type are Validate-protected against
	// the empty case and Title falls back to the event type, leaving Actor the one field
	// where last-wins could silently lose a recorded value.
	if ev.Actor != "" {
		out.Actor = ev.Actor
	}
	out.LastEventID = ev.ID
	out.Title = ev.Title
	if out.Title == "" {
		out.Title = ev.Type
	}
	// FirstSeenAt is the INFERENCE: the earliest occurred_at of any event folded onto
	// the ref, which is evidence the change must have existed by then. It is a running
	// minimum, so it does not matter which event carries it or when that event arrives.
	// The ASSERTION — a start-type event's own occurred_at — is StartedAt, set in
	// foldLifecycle below.
	if out.FirstSeenAt.IsZero() || ev.OccurredAt.Before(out.FirstSeenAt) {
		out.FirstSeenAt = ev.OccurredAt
	}
	foldLifecycle(&out, ev)

	// Start is COALESCE(StartedAt, FirstSeenAt), which is exactly what Postgres
	// generates for the start_at column (migration 0003). Computing the same value here
	// keeps the in-memory Change equal to the row the database will hold, which
	// RebuildProjection depends on: its accumulator feeds folded Change values straight
	// back in as cur.
	//
	// Both facts are minima, so both are commutative and Start is therefore insensitive
	// to fold order: a rebuild (id order) and a live append (arrival order) cannot
	// disagree about it. That is why the assertion is a minimum rather than an
	// assignment — last-one-folded-wins would make the two orders disagree whenever a
	// ref carries more than one start-type event with differing timestamps.
	out.Start = out.FirstSeenAt
	if out.StartedAt != nil {
		out.Start = *out.StartedAt
	}

	return out
}

// foldLifecycle applies ev's lifecycle effect — the assertion of a start, an end, and
// the status those imply — to out.
//
// out is a value Fold owns outright and has not returned yet, so writing through this
// pointer does not weaken Fold's purity: nothing reachable from cur or ev is mutated.
// Every time pointer assigned here is freshly allocated from a local copy for that same
// reason — out.End and out.StartedAt may both have been copied from cur, and writing
// through either would reach back into the caller's Change.
func foldLifecycle(out *Change, ev Event) {
	switch {
	case ev.CorrelationRef == "":
		at := ev.OccurredAt
		out.End = &at
		out.Status = StatusOccurred
	case isStartType(ev.Type):
		// The EARLIEST assertion wins, not the last one folded. A ref can carry more
		// than one start-type event — a retried pipeline, two producers describing one
		// deployment — and last-folded-wins would make a rebuild (id order) and a live
		// append (arrival order) disagree whenever their timestamps differ. Minimum is
		// commutative, so taking it keeps the two convergent.
		if out.StartedAt == nil || ev.OccurredAt.Before(*out.StartedAt) {
			at := ev.OccurredAt
			out.StartedAt = &at
		}
		if out.End == nil {
			out.Status = StatusInProgress
		}
	default:
		if status, terminal := StatusForType(ev.Type); terminal {
			at := ev.OccurredAt
			out.End = &at
			out.Status = status
		} else if out.End == nil {
			// A correlated event that neither starts nor ends the change —
			// incident.declared, deployment.triggered, maintenance.scheduled,
			// deployment.annotated. A change has three shapes and admits no fourth, so
			// this is in flight; leaving Status empty wrote a row that no status filter
			// could ever select and that rendered as an interval growing to the year
			// 9999. The End guard is why a late annotation cannot reopen a change that
			// has already reached a terminal status.
			out.Status = StatusInProgress
		}
	}
}
