package postgres

import (
	"fmt"

	"github.com/jackc/pgx/v5"
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
