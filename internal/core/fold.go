package core

import (
	"slices"
	"strings"
)

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

// RefFor returns the projection key for an event: its correlation ref when it has one,
// otherwise its locus and its own id, which makes it a point.
//
// Both branches yield <locus>/<something>, which is the point: a correlated ref is
// composed at ingest by resolveRef, and a point's is composed here. Composing a point's
// from the stored event — rather than prefixing the projection — is what keeps
// RebuildProjection able to reproduce it from the log alone.
//
// The point branch calls composeRef rather than NewRef because both halves are already
// validated — the locus by Validate, the id by ulid.ParseStrict — and because RefFor has no
// error return to give: Fold calls it and must stay total.
func RefFor(ev Event) Ref {
	if !ev.CorrelationRef.IsZero() {
		return ev.CorrelationRef
	}
	return composeRef(ev.Locus, ev.ID)
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

// mergedSources returns cur's sources with ev's added, deduplicated and sorted, in a fresh
// slice. Fold's purity depends on that copy for the same reason mergedAttributes needs one:
// the returned Change must never share a slice with cur.
//
// out starts as []string{}, not nil: pgx encodes a nil slice as SQL NULL, which the sources
// column's NOT NULL rejects with an error naming neither Fold nor this function. Ingest's
// Validate keeps ev.Source non-empty in production, so an empty result needs an empty
// cur.Sources too, but Fold itself gives no such guarantee, and the failure it would
// otherwise produce is worth avoiding regardless of how unreachable it is today.
//
// Sorted so the result is a function of the SET and not of arrival order, which is what
// makes a rebuild reproduce it exactly and what migration 0007's
// array_agg(DISTINCT ... ORDER BY source COLLATE "C") has to match byte-for-byte: this sort
// is byte order, not the database's default collation.
func mergedSources(cur *Change, ev Event) []string {
	seen := make(map[string]struct{}, 2)
	out := []string{}
	if cur != nil {
		for _, s := range cur.Sources {
			if _, ok := seen[s]; !ok {
				seen[s] = struct{}{}
				out = append(out, s)
			}
		}
	}
	if _, ok := seen[ev.Source]; !ok && ev.Source != "" {
		seen[ev.Source] = struct{}{}
		out = append(out, ev.Source)
	}
	slices.Sort(out)
	return out
}

// Fold applies one event to a change and returns the result. cur is nil for
// the first event of a ref, but a zero-value *Change (as produced by, e.g.,
// `c := m[ref]; m[ref] = Fold(&c, ev)` over a map[Ref]Change) must yield
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
	out.Sources = mergedSources(cur, ev)
	// Last NON-EMPTY wins. Actor is optional on both ingest paths, so an event carrying
	// none is ordinary traffic; an unconditional assignment let any such event blank the
	// actor a previous event had recorded, and a rebuild reproduced the erasure rather
	// than repairing it. Subject and Type are Validate-protected against the empty case.
	// Title has the same hazard and is guarded the same way just below.
	if ev.Actor != "" {
		out.Actor = ev.Actor
	}
	out.LastEventID = ev.ID
	// Last NON-EMPTY wins, for the same reason as Actor above. An unconditional
	// assignment did not merely blank a recorded title: the fallback below then
	// substituted the LATER event's type, turning "Deploy v2.5.0 to prod" into
	// "deployment.annotated". A plausible-looking wrong answer is worse than an empty
	// one, and it reaches the egress JSON, the Grafana annotation title and the OTLP
	// span name. Title is optional on both ingest paths, so a titleless follow-up
	// event is the common case rather than an edge case.
	//
	// The type fallback still applies when nothing has ever titled the change, so a
	// change always renders as something. KNOWN RESIDUAL: for a change no event ever
	// titles, the stored fallback is the FIRST folded event's type and later events do
	// not update it, so that case stays fold-order sensitive exactly as it was before.
	// Removing that means not storing the fallback at all and having every renderer
	// apply it — none does today, since the stored title is never empty — which changes
	// what the API returns and is out of scope here.
	if ev.Title != "" {
		out.Title = ev.Title
	} else if out.Title == "" {
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

// changeState is the two-state space foldLifecycle branches on. End == nil is exactly
// Status == in_progress, so the six statuses collapse to these two for guard purposes.
type changeState uint8

const (
	stateOpen changeState = iota
	stateClosed
	numStates
)

// assertion is what an event says about its change, once classify has resolved
// explicit-beats-convention. It is NOT the event's type and NOT its identity.
type assertion uint8

const (
	assertsNothingStandalone assertion = iota
	assertsNothingCorrelated
	assertsStart
	assertsTerminal
	numAssertions
)

// statusRule is what a cell does to the change's status. The zero value is unreachable so
// that a cell nobody wrote reads as "this pair cannot occur" rather than as "do nothing".
type statusRule uint8

const (
	unreachable statusRule = iota
	holdStatus
	setInProgress
	setOccurred
	setAsserted // the terminal status the event asserted
)

type cell struct {
	status   statusRule
	setsEnd  bool
	minStart bool
	// commutative records whether folding this rule in either order agrees. The rebuild
	// invariant needs it per rule, and a cell is the only place it can live beside the
	// rule it describes. TestFoldLifecycleCommutativeCellsAgreeInBothOrders enforces it, so
	// it is documentation that fails rather than documentation that rots.
	commutative bool
}

// transitions has a cell for every (state, assertion) pair by construction: a fixed array
// rather than a map, so no pair can be silently absent.
var transitions = [numStates][numAssertions]cell{
	stateOpen: {
		assertsNothingStandalone: {setOccurred, true, false, true},
		assertsNothingCorrelated: {setInProgress, false, false, true},
		assertsStart:             {setInProgress, false, true, true},
		assertsTerminal:          {setAsserted, true, false, false},
	},
	stateClosed: {
		assertsNothingStandalone: {}, // unreachable: see the table's standalone column
		assertsNothingCorrelated: {holdStatus, false, false, true},
		assertsStart:             {holdStatus, false, true, true},
		assertsTerminal:          {setAsserted, true, false, false},
	},
}

// classify resolves what ev asserts about its change: the explicit Lifecycle field when it
// carries one, the type's suffix otherwise. This is the only place explicit-beats-convention
// lives, and the only ordering left in the fold — ordering that belongs to READING an
// assertion rather than to APPLYING it.
//
// The standalone check is last deliberately. Having no correlation ref is an identity fact,
// not an assertion, so it is what an event falls back to when it asserts nothing at all.
func classify(ev Event) (assertion, Status) {
	switch ev.Lifecycle {
	case LifecycleStarted:
		return assertsStart, ""
	case LifecycleNone:
		// Asserted nothing. Skip the convention and fall to the identity check below.
	case LifecycleUnspecified:
		// Said nothing at all, so the producer's type vocabulary decides.
		if isStartType(ev.Type) {
			return assertsStart, ""
		}
		if status, terminal := StatusForType(ev.Type); terminal {
			return assertsTerminal, status
		}
	default:
		return assertsTerminal, statusForLifecycle(ev.Lifecycle)
	}
	if ev.CorrelationRef.IsZero() {
		return assertsNothingStandalone, ""
	}
	return assertsNothingCorrelated, ""
}

// statusForLifecycle maps the four terminal assertions onto their status. The two
// vocabularies are spelled the same deliberately; this is the single conversion point that
// keeps them from drifting apart.
func statusForLifecycle(l Lifecycle) Status {
	switch l {
	case LifecycleCompleted:
		return StatusCompleted
	case LifecycleFailed:
		return StatusFailed
	case LifecycleRolledBack:
		return StatusRolledBack
	case LifecycleCancelled:
		return StatusCancelled
	}
	// Unreachable: classify routes only terminal values here, and Validate refuses any
	// value outside Lifecycles. Returning the zero Status rather than panicking keeps
	// Fold total, which is a hard constraint.
	return ""
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
	st := stateOpen
	if out.End != nil {
		st = stateClosed
	}
	a, asserted := classify(ev)
	c := transitions[st][a]
	if c.status == unreachable {
		return
	}
	if c.minStart && (out.StartedAt == nil || ev.OccurredAt.Before(*out.StartedAt)) {
		at := ev.OccurredAt
		out.StartedAt = &at
	}
	if c.setsEnd {
		at := ev.OccurredAt
		out.End = &at
	}
	applyStatusRule(out, c, asserted)
}

// applyStatusRule sets out.Status per c's statusRule, using asserted for a cell that sets
// the terminal status the event asserted. Split out of foldLifecycle to keep its branching
// within the complexity limit.
func applyStatusRule(out *Change, c cell, asserted Status) {
	switch c.status {
	case setInProgress:
		out.Status = StatusInProgress
	case setOccurred:
		out.Status = StatusOccurred
	case setAsserted:
		out.Status = asserted
	case holdStatus:
	}
}
