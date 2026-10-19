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

// refoldCounter creates horopter.refold, labelled by whether Status or End moved.
func refoldCounter() metric.Int64Counter {
	counter, err := otel.Meter(telemetry.StoreScope).Int64Counter("horopter.refold",
		metric.WithUnit("{refold}"),
		metric.WithDescription(
			"Refs recomputed from the log, by whether the recomputed Status or End "+
				"differs from what was stored"))
	if err != nil {
		// The SDK returns a working instrument with the error, so the store opens either way.
		slog.Error("create the horopter.refold counter", "error", err)
	}
	return counter
}

// Only columns core.Fold reads, plus locus: core.RefFor composes point refs from it.
const foldColumns = `id, tenant, schema_version, source, type, subject, actor, title,
	       occurred_at, received_at, correlation_ref, attributes, locus, lifecycle`

// The resolver seam: corrections will exclude superseded events here, for both fold paths.
func foldEventQuery(where string) string {
	return `SELECT ` + foldColumns + ` FROM event WHERE ` + where + ` ORDER BY id ASC`
}

// events tells a ref that folded to nothing from one never queried.
type foldedRef struct {
	change core.Change
	events int
}

// Folds inside the scan, so memory grows with refs, not events; RefFor keys out foreign points.
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

// Queries both ref shapes; core.RefFor in foldRows decides which rows belong.
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

// The caller holds the locks. The delete branch is unreachable today but matches rebuilds.
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

// RefoldResult is a re-fold's event count and the sorted fields it changed.
type RefoldResult struct {
	Events  int
	Altered []string
}

// ErrRefNotFound reports a ref the log holds no events for.
var ErrRefNotFound = errors.New("ref has no events")

// RefoldRef re-folds one ref in its own transaction, under an append's locks.
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

	// First statement, as in AppendEvent: everything that can wait must be covered.
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
	"start", "started_at", "status", "subject", "subject_first_seen", "title",
	"title_started_at", "type",
}

// Unlike alteredFields, compares commutative fields too: those are what operators repair.
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
		{"title_started_at", !sameInstant(before.TitleStartedAt, after.TitleStartedAt)},
	} {
		if c.differs {
			moved = append(moved, c.name)
		}
	}
	slices.Sort(moved)
	return moved
}

// The eight non-commutative fields; a table keeps this under the complexity limit.
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

// Compares by value at microsecond precision: pgx truncates occurred_at on insert.
func sameInstant(a, b *time.Time) bool {
	if a == nil || b == nil {
		return a == b
	}
	return a.Truncate(time.Microsecond).Equal(b.Truncate(time.Microsecond))
}

// Compares JSON encodings: jsonb numbers decode as float64, request values may be ints.
func sameAttributes(a, b map[string]any) bool {
	ja, aErr := json.Marshal(a)
	jb, bErr := json.Marshal(b)
	if aErr != nil || bErr != nil {
		return false
	}
	return string(ja) == string(jb)
}

// Only Status and End, matching the warn log and the alert the docs recommend.
func movedSomethingRecorded(moved []string) bool {
	for _, field := range moved {
		if field == "status" || field == "end" {
			return true
		}
	}
	return false
}
