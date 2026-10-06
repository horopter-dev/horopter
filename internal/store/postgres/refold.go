package postgres

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"slices"
	"time"

	"github.com/jackc/pgx/v5"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
	"horopter.dev/horopter/internal/core"
	"horopter.dev/horopter/internal/telemetry"
)

// refoldCounter creates horopter.refold. It counts refs recomputed from the log, labelled
// by whether the recomputation moved the change's recorded OUTCOME — Status or End — not
// by whether it changed the stored change at all: a re-fold that moves Subject, Type,
// Title, Actor, Attributes or last_event_id, and neither Status nor End, still reports
// altered="false". See movedSomethingRecorded for why those two fields and no others.
func refoldCounter() metric.Int64Counter {
	counter, err := otel.Meter(telemetry.StoreScope).Int64Counter("horopter.refold",
		metric.WithUnit("{refold}"),
		metric.WithDescription(
			"Refs recomputed from the log, by whether the recomputed Status or End "+
				"differs from what was stored"))
	if err != nil {
		// The SDK returns a working instrument alongside a name-validation error, so the
		// store opens either way. The error means this call is wrong, not the environment.
		slog.Error("create the horopter.refold counter", "error", err)
	}
	return counter
}

// foldColumns names only the columns core.Fold reads, which is why origin and edited are
// absent — but locus is NOT one of those omissions. core.RefFor composes a point change's
// ref from it, so dropping it rebuilds every point change under "/<id>" and silently
// diverges from the projection AppendEvent wrote, which is the shape of the bug 0004
// shipped.
const foldColumns = `id, tenant, schema_version, source, type, subject, actor, title,
	       occurred_at, received_at, correlation_ref, attributes, locus, lifecycle`

// foldEventQuery builds the statement every fold reads its events from.
//
// THIS IS THE RESOLVER SEAM. Area B's corrections are resolved by excluding superseded
// events here, in one place, so a correction cannot resolve on the per-ref path and not
// on the whole-tenant rebuild — the divergence class that made migration 0006 necessary.
// It is deliberately a predicate rather than an in-memory pre-pass over a materialised
// event list: foldEventLog's streaming accumulator keeps peak memory proportional to the
// number of distinct refs rather than the number of events, and a list-shaped resolver
// would destroy that on a tenant rebuild.
//
// Today it adds no such clause, so there is no unused abstraction here — only one place
// for Area B to change.
func foldEventQuery(where string) string {
	return `SELECT ` + foldColumns + ` FROM event WHERE ` + where + ` ORDER BY id ASC`
}

// foldedRef is one ref's folded change and the number of events that produced it. The
// count is carried because a caller needs to tell a ref that folded to nothing from one
// that was never queried, and because replayProjection reports both figures on its span.
type foldedRef struct {
	change core.Change
	events int
}

// foldRows folds an id-ordered event scan into the projection it implies, keyed by ref.
//
// Folding inside the scan loop, rather than materialising the whole log into a slice
// first, keeps peak memory proportional to the number of distinct refs rather than the
// total number of events replayed.
//
// The accumulator passes a nil *core.Change for the first event of each ref, matching how
// AppendEvent applies events incrementally; core.Fold is written so both spellings
// converge.
//
// Keying by core.RefFor is also the membership guard. foldRef's query matches a point
// event on its id alone, so a point event whose id equals some correlated ref's producer
// half is read by that query — and then keyed under its OWN locus, which is a different
// ref, so it never reaches the change the caller asked for.
func foldRows(rows pgx.Rows) (map[core.Ref]foldedRef, error) {
	acc := map[core.Ref]foldedRef{}
	for rows.Next() {
		var (
			ev    core.Event
			corr  string
			locus string
		)
		if err := rows.Scan(&ev.ID, &ev.Tenant, &ev.SchemaVersion, &ev.Source, &ev.Type,
			&ev.Subject, &ev.Actor, &ev.Title, &ev.OccurredAt, &ev.ReceivedAt,
			&corr, &ev.Attributes, &locus, &ev.Lifecycle); err != nil {
			return nil, fmt.Errorf("scan event row: %w", err)
		}
		ev.CorrelationRef = core.RefFromRow(corr)
		ev.Locus = core.LocusFromRow(locus)
		ref := core.RefFor(ev)
		prev := acc[ref]
		var cur *core.Change
		if prev.events > 0 {
			cur = &prev.change
		}
		acc[ref] = foldedRef{change: core.Fold(cur, ev), events: prev.events + 1}
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate event rows: %w", err)
	}
	return acc, nil
}

// foldRef folds one ref from the log, reading both shapes a ref can have.
//
// A ref's membership cannot be read off the ref itself: `ci/01H…` is a correlated ref if
// events carry it in correlation_ref, and a point ref if the event with that id carries
//
//	correlation_ref = ''
//
// Both arms are queried, and core.RefFor decides which rows actually belong — see
// foldRows. (Written as a code block, not running text: gofmt rewrites two apostrophes
// into a closing smart quote in prose, which is how query_test.go's equivalent note came
// to exist.)
//
// A ref with no events returns a zero foldedRef, whose events field is 0. Under the
// mechanisms decided so far that means the ref has never existed: erasure blanks a value
// in place and does not delete the row, a correction is a complete replacement so the
// newest one always survives, and core.Event's invariant forbids deletion outright.
func foldRef(
	ctx context.Context, tx pgx.Tx, tenantID string, ref core.Ref,
) (foldedRef, error) {
	rows, err := tx.Query(ctx, foldEventQuery(
		`tenant = $1 AND (correlation_ref = $2 OR (correlation_ref = '' AND id = $3))`),
		tenantID, ref.String(), ref.ProducerRef())
	if err != nil {
		return foldedRef{}, fmt.Errorf(
			"read events for ref %q in tenant %q: %w", ref, tenantID, err)
	}
	defer rows.Close()

	acc, err := foldRows(rows)
	if err != nil {
		return foldedRef{}, fmt.Errorf(
			"fold ref %q in tenant %q: %w", ref, tenantID, err)
	}
	return acc[ref], nil
}

// refoldRef recomputes one ref's change from the log and writes the result, deleting the
// change row when the ref folds to nothing. It returns the folded ref it wrote, so a
// caller that needs the change — refoldOutOfOrderAppend does — has it in memory rather
// than reading it back from the row this call just wrote.
//
// The caller holds the locks. On the append path that is lockProjectionWrite, already
// taken; a standalone caller takes the same pair before calling this.
//
// The delete branch is UNREACHABLE under the mechanisms decided so far — see foldRef on
// why a ref that ever held an event cannot fold to nothing. It is kept for driver
// equivalence: RebuildProjection reaches that state through clearProjection and replay,
// and without the delete a per-ref re-fold and a whole-tenant rebuild would disagree
// about a ref whose events had gone. The only thing that could produce that state is the
// subject-level bulk erasure deferred with retention and compaction.
func refoldRef(
	ctx context.Context, tx pgx.Tx, tenantID string, ref core.Ref,
) (foldedRef, error) {
	folded, err := foldRef(ctx, tx, tenantID, ref)
	if err != nil {
		return foldedRef{}, err
	}
	if folded.events == 0 {
		if _, err := tx.Exec(ctx,
			`DELETE FROM change WHERE tenant = $1 AND ref = $2`,
			tenantID, ref.String()); err != nil {
			return foldedRef{}, fmt.Errorf(
				"clear change %q in tenant %q: %w", ref, tenantID, err)
		}
		return foldedRef{}, nil
	}
	if err := upsertChange(ctx, tx, folded.change); err != nil {
		return foldedRef{}, err
	}
	return folded, nil
}

// RefoldResult is what a re-fold did: how many events it read from the log for the ref, and
// which fields of the stored change it changed, sorted.
type RefoldResult struct {
	Events  int
	Altered []string
}

// ErrRefNotFound reports a ref the log holds no events for.
var ErrRefNotFound = errors.New("ref has no events")

// RefoldRef recomputes one ref's change from the log in its own transaction, taking the
// same locks an append takes so it cannot interleave with one or with a rebuild. Appends to
// every other ref carry on while it runs.
//
// It returns ErrRefNotFound when the log holds no events for the ref. An unknown ref writes
// nothing. A change row whose events have all gone is still removed, as a rebuild would —
// unreachable under the mechanisms decided so far; see refoldRef.
//
// It is the operation decisions/2026-09-16 calls the per-ref re-fold, exposed for
// operators by ingress's POST /v1/admin/refold. The append path calls refoldRef directly,
// inside the transaction it already holds.
func (s *Store) RefoldRef(
	ctx context.Context, tenantID string, ref core.Ref,
) (RefoldResult, error) {
	ctx, span := tracer().Start(ctx, "RefoldRef")
	defer span.End()

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return RefoldResult{}, fmt.Errorf("begin re-fold transaction: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	// Set as the transaction's first statement, for the reason AppendEvent sets it
	// there: everything that can wait must be covered, not just the lock call.
	if err := setLockTimeout(ctx, tx, AppendLockTimeout, "re-fold"); err != nil {
		return RefoldResult{}, err
	}
	var out appendOutcome
	if err := lockProjectionWrite(ctx, tx, tenantID, ref.String(), &out); err != nil {
		return RefoldResult{}, err
	}
	before, found, err := lockAndLoadChange(ctx, tx, tenantID, ref.String())
	if err != nil {
		return RefoldResult{}, err
	}
	folded, err := refoldRef(ctx, tx, tenantID, ref)
	if err != nil {
		return RefoldResult{}, err
	}
	span.SetAttributes(
		attribute.String("horopter.tenant", tenantID),
		attribute.String("horopter.event.ref", ref.String()),
		attribute.Int("horopter.refold.events", folded.events),
	)
	if err := tx.Commit(ctx); err != nil {
		return RefoldResult{}, fmt.Errorf(
			"commit re-fold of %q in tenant %q: %w", ref, tenantID, err)
	}
	if folded.events == 0 {
		return RefoldResult{}, fmt.Errorf(
			"re-fold %q in tenant %q: %w", ref, tenantID, ErrRefNotFound)
	}
	return RefoldResult{
		Events: folded.events, Altered: changedFields(before, found, folded.change),
	}, nil
}

// allChangeFields names every field changedFields compares, sorted.
var allChangeFields = []string{
	"actor", "attributes", "end", "first_seen_at", "last_event_id", "sources",
	"start", "started_at", "status", "subject", "subject_first_seen", "title", "type",
}

// changedFields names every field of the stored change a re-fold changed, sorted. Unlike
// alteredFields, which explains an ordering flip and so skips the commutative fields, it
// compares all of them: a hand-corrupted start or source set is what an operator re-folds
// to repair. A change that was missing from the projection reports every field. Times and
// attributes compare as alteredFields compares them, at the precision Postgres stores.
func changedFields(before core.Change, found bool, after core.Change) []string {
	if !found {
		return slices.Clone(allChangeFields)
	}
	moved := alteredFields(before, after)
	for _, c := range []struct {
		name    string
		differs bool
	}{
		{"start", !sameInstant(&before.Start, &after.Start)},
		{"started_at", !sameInstant(before.StartedAt, after.StartedAt)},
		{"first_seen_at", !sameInstant(&before.FirstSeenAt, &after.FirstSeenAt)},
		{"sources", !slices.Equal(before.Sources, after.Sources)},
		{"subject_first_seen", !maps.Equal(before.SubjectFirstSeen, after.SubjectFirstSeen)},
	} {
		if c.differs {
			moved = append(moved, c.name)
		}
	}
	slices.Sort(moved)
	return moved
}

// alteredFields names the fields a re-fold moved. They are exactly the eight whose fold
// rules are not commutative, listed in RebuildProjection's doc comment: assigned
// unconditionally (Subject, Type, LastEventID), guarded non-empty (Title, Actor), merged
// last-wins per key (Attributes), and guarded on End (Status, End). The commutative
// fields — Start, StartedAt, FirstSeenAt, Sources, SubjectFirstSeen — cannot differ
// between two orders and are deliberately absent.
//
// A table rather than a chain of ifs: eight branches would put this function over the
// project's cyclomatic limit of 8 on its own.
func alteredFields(before, after core.Change) []string {
	checks := []struct {
		name    string
		differs bool
	}{
		{"subject", before.Subject != after.Subject},
		{"type", before.Type != after.Type},
		{"title", before.Title != after.Title},
		{"actor", before.Actor != after.Actor},
		{"last_event_id", before.LastEventID != after.LastEventID},
		{"status", before.Status != after.Status},
		{"end", !sameInstant(before.End, after.End)},
		{"attributes", !sameAttributes(before.Attributes, after.Attributes)},
	}
	moved := make([]string, 0, len(checks))
	for _, c := range checks {
		if c.differs {
			moved = append(moved, c.name)
		}
	}
	return moved
}

// sameInstant compares two optional timestamps by VALUE, not by presence, and at the
// precision Postgres actually stores rather than whatever precision Go happens to be
// holding in memory.
//
// Comparing only nil-ness hides a real move: two terminal events of the same type folded
// onto one ref, in different orders, both leave End non-nil — core.Fold's terminal cell
// overwrites End every time it fires, so whichever terminal event folds LAST sets it,
// and the incremental and re-folded answers can carry different timestamps while every
// other field agrees. Clock skew across ingress replicas is exactly what makes two
// terminal events on one ref reachable, and is the whole premise of this work. That is a
// change to when the log says the work finished, and it must not be invisible.
//
// Comparing at full VALUE, without the truncation, hides the opposite failure: before is
// folded incrementally from the event as AppendEvent received it, still carrying whatever
// sub-microsecond precision the producer sent; after comes from refoldRef, which reads
// that same event's occurred_at back from Postgres — a timestamptz column, and pgx's
// binary wire encoding truncates a Go time.Time to microseconds on the way in (integer
// division of the nanosecond field by 1000). Nothing under internal/ truncates OccurredAt
// before insert, so without this, a producer emitting sub-microsecond timestamps gets
// "end" reported as moved, and horopter_refold_altered="true" fired, on every terminal
// out-of-order append — whether or not the recorded outcome actually changed. Truncating
// both sides to the granularity Postgres actually stores is sameAttributes's fix applied
// to the same class of problem: compare at the representation the two paths can agree on,
// not at whichever one happens to carry more precision. What this gives up is bounded and
// unobservable: the largest real move it can now fail to report is 999ns, and only when
// two in-memory nanosecond values fall in the same microsecond bucket — refolded.End
// always comes from occurred_at at microsecond precision, so no difference the log
// actually stores is ever hidden by this.
func sameInstant(a, b *time.Time) bool {
	if a == nil || b == nil {
		return a == b
	}
	return a.Truncate(time.Microsecond).Equal(b.Truncate(time.Microsecond))
}

// sameAttributes compares two attribute maps by the JSON value they encode, not by their Go
// representation.
//
// The two sides of alteredFields's comparison do not share one path to their Go values.
// applyToProjection's incremental fold takes an out-of-order event's OWN attribute values
// straight from the request — a Go int stays an int — for any key that event itself sets,
// while every value refoldRef reports came from foldRows scanning the event table's jsonb
// column, where a JSON number always decodes as float64. reflect.DeepEqual(1, 1.0) is
// false, so that difference alone reported "attributes" moved on every out-of-order append
// whose contested key held a JSON number, whether or not the recorded value had actually
// changed. Comparing compact JSON encodings sidesteps the Go type entirely: both sides
// render a JSON number the same way regardless of whether the value on either side is an
// int, an int64 or a float64, and encoding/json.Marshal sorts map keys, so key order cannot
// cause a false difference either.
//
// A marshal error is treated as a difference rather than swallowed. It is not expected to
// happen: insertEvent already bound this event's attributes to the jsonb column before
// applyToProjection ever runs, so anything unmarshalable would have failed there first.
func sameAttributes(a, b map[string]any) bool {
	ja, aErr := json.Marshal(a)
	jb, bErr := json.Marshal(b)
	if aErr != nil || bErr != nil {
		return false
	}
	return string(ja) == string(jb)
}

// movedSomethingRecorded reports whether a re-fold changed the change's recorded outcome —
// Status or End — as against everything else a re-fold can move.
//
// Narrowed to those two fields so the counter agrees with the warn log two lines above its
// only caller: slog.WarnContext there is titled "a re-fold moved a recorded outcome" and
// fires on that identical condition, Status or End moving. README.md documents
// horopter_refold_altered="true" as "the rate at which late arrivals are changing recorded
// outcomes, which is the number worth an alert" — Subject, Type, Title, Actor and
// Attributes are real moves alteredFields still names in horopter.refold.moved on the
// append span, but none of them is what the log said HAPPENED, and counting one here would
// teach an operator paging on this label to ignore it.
//
// last_event_id would need excluding regardless: it moves on EVERY out-of-order re-fold, by
// construction — core.Fold assigns it unconditionally, so the incremental answer is always
// the arriving event's own id (the lower one, since that is what put this append on the
// re-fold branch) while the re-folded answer is always the ref's id-ordered maximum, and the
// two can never be equal here — but it is excluded by the same allowlist as every other
// non-outcome field, not as a special case.
func movedSomethingRecorded(moved []string) bool {
	for _, field := range moved {
		if field == "status" || field == "end" {
			return true
		}
	}
	return false
}
