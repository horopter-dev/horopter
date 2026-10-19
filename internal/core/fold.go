package core

import (
	"maps"
	"slices"
	"strings"
	"time"
)

// StatusForType reports the terminal status implied by an event type's last segment, if any.
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

// RefFor returns an event's correlation ref, or composes a point's from its locus and id.
func RefFor(ev Event) Ref {
	if !ev.CorrelationRef.IsZero() {
		return ev.CorrelationRef
	}
	return composeRef(ev.Locus, ev.ID)
}

// mergedAttributes overlays ev's attributes on cur's in a fresh map, so Fold never mutates cur.
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

// mergedSources returns the union of cur's and ev's sources, sorted by byte as COLLATE "C" is.
func mergedSources(cur *Change, ev Event) []string {
	seen := make(map[string]struct{}, 2)
	// Not nil: pgx writes a nil slice as NULL, which the NOT NULL column rejects.
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

// mergedSubjectFirstSeen adds ev's subject at its earliest time, in a fresh, never-nil map.
func mergedSubjectFirstSeen(cur *Change, ev Event) map[string]int64 {
	out := map[string]int64{}
	if cur != nil {
		maps.Copy(out, cur.SubjectFirstSeen)
	}
	if ev.Subject == "" {
		return out
	}
	at := ev.OccurredAt.UnixMicro()
	if prev, ok := out[ev.Subject]; !ok || at < prev {
		out[ev.Subject] = at
	}
	return out
}

// foldTitle names the change by its earliest titled start, at Postgres's microsecond precision.
func foldTitle(out *Change, ev Event) {
	if a, _ := classify(ev); a == assertsStart && ev.Title != "" {
		at := ev.OccurredAt.Truncate(time.Microsecond)
		if out.TitleStartedAt == nil || earlierTitle(at, ev.Title, *out.TitleStartedAt, out.Title) {
			out.Title, out.TitleStartedAt = ev.Title, &at
		}
		return
	}
	if out.TitleStartedAt != nil {
		return
	}
	if ev.Title != "" {
		out.Title = ev.Title
	} else if out.Title == "" {
		out.Title = ev.Type
	}
}

// earlierTitle orders two titled starts: by time, then by title, so either fold order agrees.
func earlierTitle(at time.Time, title string, heldAt time.Time, held string) bool {
	heldAt = heldAt.Truncate(time.Microsecond)
	return at.Before(heldAt) || (at.Equal(heldAt) && title < held)
}

// Fold applies ev to cur purely; a nil cur and a zero-value one must fold alike.
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
	out.SubjectFirstSeen = mergedSubjectFirstSeen(cur, ev)
	// Last non-empty wins: an actorless event used to erase the recorded actor.
	if ev.Actor != "" {
		out.Actor = ev.Actor
	}
	out.LastEventID = ev.ID
	// Earliest titled start, else last non-empty; the type fallback stays fold-order sensitive.
	foldTitle(&out, ev)
	// FirstSeenAt is the inferred start, a running minimum; StartedAt is the asserted one.
	if out.FirstSeenAt.IsZero() || ev.OccurredAt.Before(out.FirstSeenAt) {
		out.FirstSeenAt = ev.OccurredAt
	}
	foldLifecycle(&out, ev)

	// Start mirrors the generated start_at column, which a rebuild feeds back in as cur.
	out.Start = out.FirstSeenAt
	if out.StartedAt != nil {
		out.Start = *out.StartedAt
	}

	return out
}

// changeState is open or closed: End == nil is exactly Status == in_progress.
type changeState uint8

const (
	stateOpen changeState = iota
	stateClosed
	numStates
)

// assertion is what an event says about its change once classify has resolved it.
type assertion uint8

const (
	assertsNothingStandalone assertion = iota
	assertsNothingCorrelated
	assertsStart
	assertsTerminal
	numAssertions
)

// statusRule is what a cell does to the status; its zero value marks an unreachable cell.
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
	// commutative records whether folding this rule in either order agrees.
	commutative bool
}

// transitions is a fixed array so no (state, assertion) pair can be missing.
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

// classify resolves what ev asserts: an explicit Lifecycle beats the type's suffix.
func classify(ev Event) (assertion, Status) {
	switch ev.Lifecycle {
	case LifecycleStarted:
		return assertsStart, ""
	case LifecycleNone:
		// Asserted nothing. Skip the convention and fall to the identity check below.
	case LifecycleUnspecified:
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

// statusForLifecycle maps a terminal Lifecycle onto the Status spelled the same.
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
	// Unreachable for a validated event; returning rather than panicking keeps Fold total.
	return ""
}

// foldLifecycle applies ev's start, end and status, assigning fresh pointers so cur is untouched.
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

// applyStatusRule sets out.Status per c's rule, using asserted for setAsserted.
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
