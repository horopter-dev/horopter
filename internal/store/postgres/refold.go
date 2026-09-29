package postgres

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5"
	"go.opentelemetry.io/otel/attribute"
	"horopter.dev/horopter/internal/core"
)

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
// change row when the ref folds to nothing. It returns the number of events folded.
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
) (int, error) {
	folded, err := foldRef(ctx, tx, tenantID, ref)
	if err != nil {
		return 0, err
	}
	if folded.events == 0 {
		if _, err := tx.Exec(ctx,
			`DELETE FROM change WHERE tenant = $1 AND ref = $2`,
			tenantID, ref.String()); err != nil {
			return 0, fmt.Errorf(
				"clear change %q in tenant %q: %w", ref, tenantID, err)
		}
		return 0, nil
	}
	if err := upsertChange(ctx, tx, folded.change); err != nil {
		return 0, err
	}
	return folded.events, nil
}

// RefoldRef recomputes one ref's change from the log in its own transaction, taking the
// same locks an append takes so it cannot interleave with one or with a rebuild.
//
// It is the operation decisions/2026-09-16 calls the per-ref re-fold, exposed for
// operators and for Area B's management API. The append path calls refoldRef directly,
// inside the transaction it already holds.
func (s *Store) RefoldRef(ctx context.Context, tenantID string, ref core.Ref) (err error) {
	ctx, span := tracer().Start(ctx, "RefoldRef")
	defer span.End()

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin re-fold transaction: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	// Set as the transaction's first statement, for the reason AppendEvent sets it
	// there: everything that can wait must be covered, not just the lock call.
	if err := setLockTimeout(ctx, tx, AppendLockTimeout, "re-fold"); err != nil {
		return err
	}
	var out appendOutcome
	if err := lockProjectionWrite(ctx, tx, tenantID, ref.String(), &out); err != nil {
		return err
	}
	events, err := refoldRef(ctx, tx, tenantID, ref)
	if err != nil {
		return err
	}
	span.SetAttributes(
		attribute.String("horopter.tenant", tenantID),
		attribute.String("horopter.event.ref", ref.String()),
		attribute.Int("horopter.refold.events", events),
	)
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit re-fold of %q in tenant %q: %w", ref, tenantID, err)
	}
	return nil
}
