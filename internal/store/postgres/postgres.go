// Package postgres implements Horopter's persistence on PostgreSQL.
//
// Postgres is the only backend. Timestamps are native timestamptz, so ordering and
// range comparison are temporal rather than lexicographic, and attributes are JSONB.
package postgres

import (
	"context"
	"embed"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/yo61/horopter/internal/core"
)

//go:embed migrations/*.sql
var migrationFS embed.FS

// migrationLockKey is an arbitrary but fixed application id for pg_advisory_xact_lock.
// Every process that migrates uses this same key, which is what serialises them.
const migrationLockKey int64 = 8145209847321

// Store is the PostgreSQL-backed event store.
type Store struct{ pool *pgxpool.Pool }

// Pool exposes the connection pool for tests and for callers needing raw access.
func (s *Store) Pool() *pgxpool.Pool { return s.pool }

// Close releases the pool.
func (s *Store) Close() { s.pool.Close() }

// Open connects to dsn and applies all migrations.
func Open(ctx context.Context, dsn string) (*Store, error) {
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		return nil, fmt.Errorf("connect to postgres: %w; check the dsn is a valid "+
			"postgres connection string", err)
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("ping postgres: %w; is the database reachable?", err)
	}
	if err := Migrate(ctx, pool); err != nil {
		pool.Close()
		return nil, err
	}
	return &Store{pool: pool}, nil
}

// Migrate applies every embedded migration inside one transaction, guarded by a
// transaction-scoped advisory lock.
//
// The lock is what makes this safe when several daemons start at once, as they do on
// every rolling deploy: without it, concurrent CREATE INDEX statements race and can
// leave a partially applied schema with no error surfaced to the operator.
//
// pg_advisory_xact_lock, rather than pg_advisory_lock, is deliberate: it is released
// automatically by Postgres when the transaction commits or rolls back, so there is no
// manual unlock call that can be skipped by a cancelled context or a swallowed error and
// leave the lock held by a connection quietly returned to the pool. Postgres DDL is
// transactional, so wrapping every migration statement in the same transaction is also
// what makes a mid-file failure roll back cleanly instead of leaving a partially applied
// schema.
func Migrate(ctx context.Context, pool *pgxpool.Pool) error {
	tx, err := pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin migration transaction: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock($1)`, migrationLockKey); err != nil {
		return fmt.Errorf("take migration advisory lock: %w", err)
	}

	entries, err := migrationFS.ReadDir("migrations")
	if err != nil {
		return fmt.Errorf("read embedded migrations: %w", err)
	}
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		names = append(names, e.Name())
	}
	sort.Strings(names)

	for _, name := range names {
		stmt, err := migrationFS.ReadFile("migrations/" + name)
		if err != nil {
			return fmt.Errorf("read migration %q: %w", name, err)
		}
		if _, err := tx.Exec(ctx, string(stmt)); err != nil {
			return fmt.Errorf("apply migration %q: %w", name, err)
		}
	}

	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit migration transaction: %w", err)
	}
	return nil
}

// ChangeQuery selects changes overlapping a time window.
type ChangeQuery struct {
	Tenant  string
	From    time.Time
	To      time.Time
	Subject string
	Type    string
	Status  string
	Limit   int
}

// EventQuery selects raw events for audit, export, and debugging.
type EventQuery struct {
	Tenant string
	Ref    string
	Limit  int
}

// AppendEvent stores an event and folds it into the projection in one transaction.
//
// A duplicate idempotency key is not an error: CI retries are normal traffic, so the
// stored event is returned with created=false.
func (s *Store) AppendEvent(ctx context.Context, ev core.Event) (core.Event, bool, error) {
	if err := ev.Validate(); err != nil {
		return core.Event{}, false, err
	}

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return core.Event{}, false, fmt.Errorf("begin transaction: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	if ev.IdempotencyKey != "" {
		existing, found, err := findByIdemKey(ctx, tx, ev.Tenant, ev.IdempotencyKey)
		if err != nil {
			return core.Event{}, false, err
		}
		if found {
			return existing, false, tx.Commit(ctx)
		}
	}

	var idem *string
	if ev.IdempotencyKey != "" {
		idem = &ev.IdempotencyKey
	}
	if _, err := tx.Exec(ctx, `
		INSERT INTO event (id, tenant, idempotency_key, schema_version, source, type,
		                   subject, actor, title, occurred_at, received_at,
		                   correlation_ref, attributes)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13)`,
		ev.ID, ev.Tenant, idem, ev.SchemaVersion, ev.Source, ev.Type,
		ev.Subject, ev.Actor, ev.Title, ev.OccurredAt.UTC(), ev.ReceivedAt.UTC(),
		ev.CorrelationRef, ev.Attributes,
	); err != nil {
		return core.Event{}, false, fmt.Errorf("insert event %q: %w", ev.ID, err)
	}

	if err := applyToProjection(ctx, tx, ev); err != nil {
		return core.Event{}, false, err
	}
	if err := tx.Commit(ctx); err != nil {
		return core.Event{}, false, fmt.Errorf("commit event %q: %w", ev.ID, err)
	}
	return ev, true, nil
}

// findByIdemKey returns the previously stored event for (tenant, idempotencyKey), if any.
func findByIdemKey(
	ctx context.Context, tx pgx.Tx, tenantID, idempotencyKey string,
) (core.Event, bool, error) {
	var (
		ev   core.Event
		idem *string
	)
	err := tx.QueryRow(ctx, `
		SELECT id, tenant, idempotency_key, schema_version, source, type, subject,
		       actor, title, occurred_at, received_at, correlation_ref, attributes
		FROM event WHERE tenant = $1 AND idempotency_key = $2`,
		tenantID, idempotencyKey,
	).Scan(&ev.ID, &ev.Tenant, &idem, &ev.SchemaVersion, &ev.Source, &ev.Type, &ev.Subject,
		&ev.Actor, &ev.Title, &ev.OccurredAt, &ev.ReceivedAt, &ev.CorrelationRef, &ev.Attributes)
	if errors.Is(err, pgx.ErrNoRows) {
		return core.Event{}, false, nil
	}
	if err != nil {
		return core.Event{}, false, fmt.Errorf(
			"find event by idempotency key %q/%q: %w", tenantID, idempotencyKey, err)
	}
	if idem != nil {
		ev.IdempotencyKey = *idem
	}
	return ev, true, nil
}

// afterLoadHook is nil in production. Tests set it to widen the window between the
// projection read and its write, making the lost-update race deterministic instead
// of dependent on machine timing.
var afterLoadHook func()

// applyToProjection folds one event onto its change, holding a row lock for the
// duration of the read-modify-write.
//
// The lock is load-bearing: ingress runs with several replicas, and without it two
// events for the same ref can be read, folded and written concurrently, with the
// later write discarding the earlier event's contribution and raising no error.
func applyToProjection(ctx context.Context, tx pgx.Tx, ev core.Event) error {
	ref := core.RefFor(ev)
	cur, found, err := lockAndLoadChange(ctx, tx, ev.Tenant, ref)
	if err != nil {
		return err
	}
	if afterLoadHook != nil {
		afterLoadHook()
	}
	var prev *core.Change
	if found {
		prev = &cur
	}
	return upsertChange(ctx, tx, core.Fold(prev, ev))
}

// lockAndLoadChange reads a change FOR UPDATE, serialising concurrent folds of the
// same ref. Returns found=false when the row does not exist yet; the subsequent
// INSERT ... ON CONFLICT then resolves the insert race.
func lockAndLoadChange(
	ctx context.Context, tx pgx.Tx, tenantID, ref string,
) (core.Change, bool, error) {
	var (
		c     core.Change
		endAt *time.Time
		st    string
	)
	err := tx.QueryRow(ctx, `
		SELECT tenant, ref, subject, type, title, actor, start_at, end_at,
		       status, attributes, last_event_id
		FROM change WHERE tenant = $1 AND ref = $2 FOR UPDATE`,
		tenantID, ref,
	).Scan(&c.Tenant, &c.Ref, &c.Subject, &c.Type, &c.Title, &c.Actor,
		&c.Start, &endAt, &st, &c.Attributes, &c.LastEventID)
	if errors.Is(err, pgx.ErrNoRows) {
		return core.Change{}, false, nil
	}
	if err != nil {
		return core.Change{}, false, fmt.Errorf("lock change %q/%q: %w", tenantID, ref, err)
	}
	c.End = endAt
	c.Status = core.Status(st)
	return c, true, nil
}

func upsertChange(ctx context.Context, tx pgx.Tx, c core.Change) error {
	_, err := tx.Exec(ctx, `
		INSERT INTO change (tenant, ref, subject, type, title, actor, start_at, end_at,
		                    end_effective, status, attributes, last_event_id)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12)
		ON CONFLICT (tenant, ref) DO UPDATE SET
		  subject=EXCLUDED.subject, type=EXCLUDED.type, title=EXCLUDED.title,
		  actor=EXCLUDED.actor, start_at=EXCLUDED.start_at, end_at=EXCLUDED.end_at,
		  end_effective=EXCLUDED.end_effective, status=EXCLUDED.status,
		  attributes=EXCLUDED.attributes, last_event_id=EXCLUDED.last_event_id`,
		c.Tenant, c.Ref, c.Subject, c.Type, c.Title, c.Actor,
		c.Start.UTC(), c.End, c.EndEffective().UTC(), string(c.Status),
		c.Attributes, c.LastEventID,
	)
	if err != nil {
		return fmt.Errorf("upsert change %q/%q: %w", c.Tenant, c.Ref, err)
	}
	return nil
}

// ListEvents selects raw events for a tenant, optionally scoped to one correlation
// ref, ordered by id ascending (which is chronological, since ids are ULIDs).
func (s *Store) ListEvents(ctx context.Context, q EventQuery) ([]core.Event, error) {
	query := `
		SELECT id, tenant, idempotency_key, schema_version, source, type, subject,
		       actor, title, occurred_at, received_at, correlation_ref, attributes
		FROM event WHERE tenant = $1`
	args := []any{q.Tenant}

	if q.Ref != "" {
		args = append(args, q.Ref)
		query += fmt.Sprintf(" AND correlation_ref = $%d", len(args))
	}
	query += " ORDER BY id ASC"
	if q.Limit != 0 {
		args = append(args, q.Limit)
		query += fmt.Sprintf(" LIMIT $%d", len(args))
	}

	rows, err := s.pool.Query(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("list events for tenant %q: %w", q.Tenant, err)
	}
	defer rows.Close()

	var evs []core.Event
	for rows.Next() {
		var (
			ev   core.Event
			idem *string
		)
		if err := rows.Scan(&ev.ID, &ev.Tenant, &idem, &ev.SchemaVersion, &ev.Source, &ev.Type,
			&ev.Subject, &ev.Actor, &ev.Title, &ev.OccurredAt, &ev.ReceivedAt,
			&ev.CorrelationRef, &ev.Attributes); err != nil {
			return nil, fmt.Errorf("scan event row for tenant %q: %w", q.Tenant, err)
		}
		if idem != nil {
			ev.IdempotencyKey = *idem
		}
		evs = append(evs, ev)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate events for tenant %q: %w", q.Tenant, err)
	}
	return evs, nil
}

// QueryChanges returns changes overlapping [q.From, q.To].
//
// The predicate is overlap, not start-containment. Filtering on start alone would hide
// a change that began before the window and is still running — precisely the change
// someone most needs to see when zooming into a spike.
func (s *Store) QueryChanges(ctx context.Context, q ChangeQuery) ([]core.Change, error) {
	sb := strings.Builder{}
	sb.WriteString(`
		SELECT tenant, ref, subject, type, title, actor, start_at, end_at,
		       status, attributes, last_event_id
		FROM change
		WHERE tenant = $1 AND start_at <= $2 AND end_effective >= $3`)
	args := []any{q.Tenant, q.To.UTC(), q.From.UTC()}

	if q.Subject != "" {
		args = append(args, q.Subject)
		fmt.Fprintf(&sb, " AND subject = $%d", len(args))
	}
	if q.Type != "" {
		args = append(args, q.Type)
		fmt.Fprintf(&sb, " AND type = $%d", len(args))
	}
	if q.Status != "" {
		args = append(args, q.Status)
		fmt.Fprintf(&sb, " AND status = $%d", len(args))
	}
	sb.WriteString(" ORDER BY start_at ASC, ref ASC")
	if q.Limit > 0 {
		args = append(args, q.Limit)
		fmt.Fprintf(&sb, " LIMIT $%d", len(args))
	}

	rows, err := s.pool.Query(ctx, sb.String(), args...)
	if err != nil {
		return nil, fmt.Errorf("query changes for tenant %q: %w", q.Tenant, err)
	}
	defer rows.Close()

	var out []core.Change
	for rows.Next() {
		var (
			c     core.Change
			endAt *time.Time
			st    string
		)
		if err := rows.Scan(&c.Tenant, &c.Ref, &c.Subject, &c.Type, &c.Title,
			&c.Actor, &c.Start, &endAt, &st, &c.Attributes, &c.LastEventID); err != nil {
			return nil, fmt.Errorf("scan change row: %w", err)
		}
		c.End = endAt
		c.Status = core.Status(st)
		out = append(out, c)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate change rows: %w", err)
	}
	return out, nil
}

// RebuildProjection discards a tenant's projection and replays its event log in ULID
// order.
//
// Because Fold is pure and ids are time-sortable, this reproduces the projection
// exactly — which is what makes the read side safe to evolve. Note the accumulator
// passes a nil *Change for the first event of each ref, matching how AppendEvent
// applies events incrementally; Fold is written so both spellings converge.
func (s *Store) RebuildProjection(ctx context.Context, tenantID string) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin rebuild transaction: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	if _, err := tx.Exec(ctx, `DELETE FROM change WHERE tenant = $1`, tenantID); err != nil {
		return fmt.Errorf("clear projection for tenant %q: %w", tenantID, err)
	}

	rows, err := tx.Query(ctx, `
		SELECT id, tenant, schema_version, source, type, subject, actor, title,
		       occurred_at, received_at, correlation_ref, attributes
		FROM event WHERE tenant = $1 ORDER BY id ASC`, tenantID)
	if err != nil {
		return fmt.Errorf("read event log for tenant %q: %w", tenantID, err)
	}
	defer rows.Close()

	// Folding directly inside the scan loop, rather than materialising the whole event
	// log into a slice first, keeps peak memory proportional to the number of distinct
	// refs rather than the total number of events replayed.
	acc := map[string]core.Change{}
	for rows.Next() {
		var ev core.Event
		if err := rows.Scan(&ev.ID, &ev.Tenant, &ev.SchemaVersion, &ev.Source, &ev.Type,
			&ev.Subject, &ev.Actor, &ev.Title, &ev.OccurredAt, &ev.ReceivedAt,
			&ev.CorrelationRef, &ev.Attributes); err != nil {
			return fmt.Errorf("scan event row for tenant %q: %w", tenantID, err)
		}
		ref := core.RefFor(ev)
		var prev *core.Change
		if c, ok := acc[ref]; ok {
			prev = &c
		}
		acc[ref] = core.Fold(prev, ev)
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("iterate event log for tenant %q: %w", tenantID, err)
	}
	rows.Close()

	for _, c := range acc {
		if err := upsertChange(ctx, tx, c); err != nil {
			return err
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit rebuild for tenant %q: %w", tenantID, err)
	}
	return nil
}
