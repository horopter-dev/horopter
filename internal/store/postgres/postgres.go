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
	"log/slog"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/exaring/otelpgx"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/yo61/horopter/internal/core"
	"github.com/yo61/horopter/internal/telemetry"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/metric"
	semconv "go.opentelemetry.io/otel/semconv/v1.43.0"
	"go.opentelemetry.io/otel/trace"
)

//go:embed migrations/*.sql
var migrationFS embed.FS

// migrationLockKey is an arbitrary but fixed application id for pg_advisory_xact_lock.
// Every process that migrates uses this same key, which is what serialises them.
const migrationLockKey int64 = 8145209847321

// uniqueViolation is the SQLSTATE Postgres raises for a unique constraint or unique
// index violation. github.com/jackc/pgerrcode names every code, but it is a separate
// module and this store needs exactly one of them.
const uniqueViolation = "23505"

// lockNotAvailable is the SQLSTATE Postgres raises when lock_timeout expires while a
// statement waits for a lock. It is matched so an operator reading a 503's server-side log
// line can tell "a rebuild is running" from "the database is down".
const lockNotAvailable = "55P03"

// AppendLockTimeout bounds how long an append waits for the projection locks.
//
// RebuildProjection holds a tenant's lock exclusively for its whole duration, so without a
// bound every append for that tenant queues behind it, each one pinning a connection from
// a pool shared across tenants: a long rebuild stalls ingress for tenants that have
// nothing to do with it. Failing fast instead turns that into a 503 the producer retries
// once the rebuild finishes — the event is delayed rather than lost, which is strictly the
// better failure.
//
// Three seconds is two orders of magnitude above ordinary same-ref contention (two appends
// to one ref is a few milliseconds of read-modify-write), so a rebuild is the usual cause
// when it does fire — but it is not the only one a bound of this size can reach. Anything
// else holding the tenant or ref lock long enough gets here too: a pathologically slow
// append, a hung transaction, a database under enough load that milliseconds become
// seconds. The value is also well inside a normal HTTP client timeout, so the producer
// receives a status it can act on rather than a client-side timeout carrying none.
const AppendLockTimeout = 3 * time.Second

// RebuildLockTimeout bounds how long a rebuild waits for the tenant lock.
//
// It is longer than AppendLockTimeout because what a rebuild waits for is in-flight
// appends finishing — milliseconds each — so reaching this bound means the tenant is under
// sustained write load rather than that anything is wrong. Giving up then, with a message
// telling the operator to retry, beats queueing indefinitely behind a stream of appends.
const RebuildLockTimeout = 10 * time.Second

// idemConstraint is the unique index enforcing one event per (tenant, idempotency_key).
// A 23505 is matched against this name rather than against the error text, so a
// different unique violation — a duplicate event id, say — is still reported as the
// error it is instead of being mistaken for a retry.
const idemConstraint = "uq_event_idem"

// querier is the QueryRow subset that both pgx.Tx and *pgxpool.Pool provide, so
// findByIdemKey can run either inside the append transaction or on the pool once a
// unique violation has aborted that transaction.
type querier interface {
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

// Store is the PostgreSQL-backed event store.
type Store struct {
	pool              *pgxpool.Pool
	appends           metric.Int64Counter
	maxAttributeBytes int
}

// Option configures a Store as it opens.
type Option func(*Store)

// WithMaxEventAttributeBytes makes AppendEvent refuse, as an invalid event, one whose
// attributes are larger than n bytes of JSON as core.Event.CheckAttributeSize measures them.
// A store opened without it applies no cap: horopter-migrate never appends, and
// horopter-ingress passes the cap its operator configured.
func WithMaxEventAttributeBytes(n int) Option {
	return func(s *Store) { s.maxAttributeBytes = n }
}

// storeFromPool wraps an open pool and applies opts. The append counter is created here, from
// the global meter provider as it stands when the store opens — which is why every binary
// calls telemetry.Setup before opening a store.
func storeFromPool(pool *pgxpool.Pool, opts []Option) *Store {
	s := &Store{pool: pool, appends: appendCounter()}
	for _, opt := range opts {
		opt(s)
	}
	return s
}

// appendCounter creates horopter.event.appends. It counts events submitted to AppendEvent,
// not changes; see appendOutcome.metricAttributes for what it is attributed by.
func appendCounter() metric.Int64Counter {
	counter, err := otel.Meter(telemetry.StoreScope).Int64Counter("horopter.event.appends",
		metric.WithUnit("{append}"),
		metric.WithDescription("Events submitted to the store, by outcome"))
	if err != nil {
		// The SDK returns a working instrument alongside a name-validation error, so the
		// store opens either way. The error means this call is wrong, not the environment.
		slog.Error("create the horopter.event.appends counter", "error", err)
	}
	return counter
}

// Pool exposes the connection pool for tests and for callers needing raw access.
func (s *Store) Pool() *pgxpool.Pool { return s.pool }

// Close releases the pool.
func (s *Store) Close() { s.pool.Close() }

// connect opens a pool for dsn and confirms it is reachable, closing the pool again if
// the ping fails. Open and OpenNoMigrate both need exactly this sequence before they
// diverge, so it lives here once rather than as two copies that a later change (a
// timeout, a retry, a pool setting) could silently update on only one path.
//
// connectErrHint is the caller-specific, operator-facing suggestion appended to a
// connect failure; the ping failure message is identical for both callers, so it is
// not parameterised.
func connect(ctx context.Context, dsn, connectErrHint string) (*pgxpool.Pool, error) {
	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		return nil, fmt.Errorf("parse the postgres dsn: %w; %s", err, connectErrHint)
	}
	// otelpgx traces each statement. Bound values are NOT included: they carry event
	// payloads, and query parameters are exactly where user data would leak into
	// telemetry. The statement text itself is this project's own SQL and is what makes
	// a span identifiable. poolTracer wraps it to report connection waits to egress.
	cfg.ConnConfig.Tracer = poolTracer{Tracer: otelpgx.NewTracer()}
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		return nil, fmt.Errorf("connect to postgres: %w; %s", err, connectErrHint)
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("ping postgres: %w; is the database reachable?", err)
	}
	recordPoolStats(pool)
	return pool, nil
}

// poolName names the pool on pgxpool.* metrics in place of otelpgx's default,
// host:port/database, which would publish the database's address and name on /metrics.
const poolName = "horopter"

// recordPoolStats exports the pool's statistics as pgxpool.* metrics through the global
// meter provider as it stands now. It runs only once the pool has answered a ping, so a
// pool that failed to connect registers nothing. A registration failure is logged: pool
// statistics are not worth refusing to open a store over.
func recordPoolStats(pool *pgxpool.Pool) {
	err := otelpgx.RecordStats(pool,
		otelpgx.WithStatsAttributes(semconv.DBClientConnectionPoolName(poolName)))
	if err != nil {
		slog.Error("record connection pool statistics; continuing without them", "error", err)
	}
}

// tracer returns the store's own instrumentation scope. Spans from this scope are the
// four public operations below; per-statement spans come from otelpgx under its own
// scope, so the two are separately switchable.
func tracer() trace.Tracer { return otel.Tracer(telemetry.StoreScope) }

// Open connects to dsn and applies all migrations.
func Open(ctx context.Context, dsn string, opts ...Option) (*Store, error) {
	pool, err := connect(ctx, dsn, "check the dsn is a valid postgres connection string")
	if err != nil {
		return nil, err
	}
	if err := Migrate(ctx, pool); err != nil {
		pool.Close()
		return nil, err
	}
	return storeFromPool(pool, opts), nil
}

// Migrate applies the embedded migrations a database has not seen yet, inside one
// transaction, guarded by a transaction-scoped advisory lock.
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
//
// applied_migration is the ledger that makes "has not seen yet" meaningful. Without it
// every file re-executes on every run, which happens to be harmless only while every
// migration is written with IF NOT EXISTS; the first backfill UPDATE, RENAME COLUMN or
// data transformation would either error on the second run or, in a system of record,
// silently corrupt data by re-applying. A database migrated before the ledger existed
// simply has an empty one, so every file re-executes once and is recorded thereafter.
//
// That recovery is safe for 0001 alone, and only because every statement in it is written
// IF NOT EXISTS. 0001 is also the only file that predates the ledger — 0002 shipped in the
// same commit as applied_migration, so no database has ever held 0002 without a ledger
// row for it. Neither 0002 nor 0003 is idempotent: 0002's ADD CONSTRAINT
// change_status_vocabulary fails outright on a second run, which is what
// TestMigrationLedgerAppliesEachFileOnce demonstrates. So restoring a backup taken before
// the ledger, or dropping applied_migration to "start again", produces a database whose
// migration cannot start: the operator has to insert a row for every file the schema
// already carries before running this again, not let them replay.
func Migrate(ctx context.Context, pool *pgxpool.Pool) error {
	tx, err := pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin migration transaction: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock($1)`, migrationLockKey); err != nil {
		return fmt.Errorf("take migration advisory lock: %w", err)
	}
	if _, err := tx.Exec(ctx, `
		CREATE TABLE IF NOT EXISTS applied_migration (
		    filename   TEXT PRIMARY KEY,
		    applied_at TIMESTAMPTZ NOT NULL DEFAULT now()
		)`); err != nil {
		return fmt.Errorf("create the applied_migration ledger: %w", err)
	}

	names, err := migrationNames()
	if err != nil {
		return err
	}
	if err := applyPendingMigrations(ctx, tx, names); err != nil {
		return err
	}
	if err := recordSchemaVersion(ctx, tx); err != nil {
		return err
	}

	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit migration transaction: %w", err)
	}
	return nil
}

// migrationNames lists the embedded migration filenames in the order they must run.
func migrationNames() ([]string, error) {
	entries, err := migrationFS.ReadDir("migrations")
	if err != nil {
		return nil, fmt.Errorf("read embedded migrations: %w", err)
	}
	if len(entries) == 0 {
		return nil, errors.New(
			"no embedded migrations found; migrations/*.sql must ship inside the binary")
	}
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		names = append(names, e.Name())
	}
	sort.Strings(names)
	return names, nil
}

// readLedger returns the set of migration filenames the database has already applied.
func readLedger(ctx context.Context, tx pgx.Tx) (map[string]bool, error) {
	rows, err := tx.Query(ctx, `SELECT filename FROM applied_migration`)
	if err != nil {
		return nil, fmt.Errorf("read the applied_migration ledger: %w", err)
	}
	defer rows.Close()

	applied := map[string]bool{}
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return nil, fmt.Errorf("scan the applied_migration ledger: %w", err)
		}
		applied[name] = true
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate the applied_migration ledger: %w", err)
	}
	return applied, nil
}

// applyPendingMigrations executes, in order, every named migration the ledger does not
// already record, adding each to the ledger as it goes.
func applyPendingMigrations(ctx context.Context, tx pgx.Tx, names []string) error {
	applied, err := readLedger(ctx, tx)
	if err != nil {
		return err
	}

	for _, name := range names {
		if applied[name] {
			continue
		}
		stmt, err := migrationFS.ReadFile("migrations/" + name)
		if err != nil {
			return fmt.Errorf("read migration %q: %w", name, err)
		}
		if _, err := tx.Exec(ctx, string(stmt)); err != nil {
			return fmt.Errorf("apply migration %q: %w", name, err)
		}
		if _, err := tx.Exec(ctx,
			`INSERT INTO applied_migration (filename) VALUES ($1)`, name); err != nil {
			return fmt.Errorf("record migration %q in the ledger: %w", name, err)
		}
	}
	return nil
}

// recordSchemaVersion writes the version implied by the highest migration filename the
// LEDGER holds. It must run after applyPendingMigrations, inside the same transaction, so
// it reads the ledger this run has just finished extending.
//
// The ledger is the authority, not the binary's embedded list. An older horopter-migrate
// run against a newer database embeds fewer migrations, so recording its own highest
// embedded file rewrote schema_version DOWNWARD while applied_migration still listed every
// file actually applied — leaving the recorded version contradicting the database's real
// structure, with nothing raised. Reading the ledger makes that binary record the true,
// higher version instead, so its own ExpectedSchemaVersion check fails loudly and the
// operator learns they are running the wrong binary.
//
// Deriving the number at all, rather than hardcoding it, is what stops a second kind of
// drift: the version used to be a literal INSERT inside 0001_init.sql, so adding a
// migration without editing that literal left the database claiming a version the schema
// no longer matched. ExpectedSchemaVersion stays a Go constant precisely so a daemon can
// assert against this value and refuse to run on a schema it does not understand.
func recordSchemaVersion(ctx context.Context, tx pgx.Tx) error {
	var latest *string
	if err := tx.QueryRow(ctx,
		`SELECT max(filename) FROM applied_migration`).Scan(&latest); err != nil {
		return fmt.Errorf("read the highest applied migration from the ledger: %w", err)
	}
	if latest == nil {
		return errors.New(
			"the applied_migration ledger is empty after applying migrations, so no " +
				"schema version can be derived; migrationNames rejects an empty embedded " +
				"set, so this should be unreachable — report it rather than working around it")
	}
	v, err := schemaVersionFromFilename(*latest)
	if err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `
		INSERT INTO schema_version (version) VALUES ($1)
		ON CONFLICT (singleton) DO UPDATE SET version = $1, applied_at = now()`, v); err != nil {
		return fmt.Errorf("record schema version %d: %w", v, err)
	}
	return nil
}

// schemaVersionFromFilename reads the leading digits of a migration filename.
func schemaVersionFromFilename(name string) (int, error) {
	digits := name
	if i := strings.IndexFunc(name, func(r rune) bool { return r < '0' || r > '9' }); i >= 0 {
		digits = name[:i]
	}
	v, err := strconv.Atoi(digits)
	if err != nil || v <= 0 {
		return 0, fmt.Errorf(
			"migration %q has no positive numeric prefix; name migrations "+
				"NNNN_description.sql so the schema version can be derived from them", name)
	}
	return v, nil
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
	// MaxRowTextBytes cuts the result after the leading rows whose row text, summed in order,
	// fits within it; zero means no cut. Row text is a row's size as Postgres renders the whole
	// row as text, octet_length(row::text). A cut that discards rows reports the result
	// truncated.
	MaxRowTextBytes int64
}

// EventQuery selects raw events for audit, export, and debugging.
type EventQuery struct {
	Tenant string
	Ref    string
	Limit  int
	// MaxRowTextBytes cuts the result after the leading rows whose row text, summed in order,
	// fits within it; zero means no cut. Row text is a row's size as Postgres renders the whole
	// row as text, octet_length(row::text). A cut that discards rows reports the result
	// truncated.
	MaxRowTextBytes int64
}

// ReadEstimate is the size of what a read will fetch, measured before it runs: how many rows,
// their total row text, and the first row's row text, in the read's order.
type ReadEstimate struct {
	Rows          int
	TotalBytes    int64
	FirstRowBytes int64
}

// overFetch returns the LIMIT to send for a caller-requested limit, and reports whether a
// limit applies at all. It is the caller's limit plus one: fetching a row beyond what was
// asked for is what makes truncation DETECTABLE, because counting to the limit alone
// cannot tell a result set of exactly Limit rows from one with more behind it. A limit of
// zero or less means "no limit", for which no LIMIT clause is emitted.
func overFetch(limit int) (int, bool) {
	if limit <= 0 {
		return 0, false
	}
	return limit + 1, true
}

// trimToLimit discards the over-fetched row and reports that the result was truncated.
// The extra row never reaches the caller, so ChangeQuery.Limit and EventQuery.Limit still
// mean exactly what the caller asked for.
func trimToLimit[T any](rows []T, limit int) ([]T, bool) {
	if limit <= 0 || len(rows) <= limit {
		return rows, false
	}
	return rows[:limit], true
}

// selection is SQL selecting whole rows of one table for a read — its tenant, filters, order
// and over-fetch limit — with the arguments it takes and the order its columns are sorted by.
// A read's query, its estimate and its byte cut all select from the same selection, which is
// what makes an estimate describe exactly the rows the read then fetches.
type selection struct {
	sql   string
	args  []any
	order string
}

// changeColumns are the columns scanChanges reads, in its order.
const changeColumns = `tenant, ref, subject, type, title, actor, start_at, started_at,
	first_seen_at, end_at, status, attributes, last_event_id`

// eventColumns are the columns scanEvents reads, in its order.
const eventColumns = `id, tenant, idempotency_key, schema_version, source, type, subject,
	actor, title, occurred_at, received_at, correlation_ref, attributes`

// changeSelection selects the change rows q reads.
func changeSelection(q ChangeQuery) selection {
	sb := strings.Builder{}
	sb.WriteString(`
		SELECT c.* FROM change c
		WHERE c.tenant = $1 AND c.start_at <= $2 AND c.end_effective >= $3`)
	args := appendChangeFilters(&sb, []any{q.Tenant, q.To.UTC(), q.From.UTC()}, q)
	sb.WriteString(" ORDER BY c.start_at ASC, c.ref ASC")
	if limit, bounded := overFetch(q.Limit); bounded {
		args = append(args, limit)
		fmt.Fprintf(&sb, " LIMIT $%d", len(args))
	}
	return selection{sql: sb.String(), args: args, order: "start_at ASC, ref ASC"}
}

// eventSelection selects the event rows q reads. See ListEvents for the ref predicate.
func eventSelection(q EventQuery) selection {
	query := `SELECT e.* FROM event e WHERE e.tenant = $1`
	args := []any{q.Tenant}
	if q.Ref != "" {
		args = append(args, q.Ref)
		query += fmt.Sprintf(
			" AND (e.correlation_ref = $%d OR (e.correlation_ref = '' AND e.id = $%d))",
			len(args), len(args))
	}
	query += " ORDER BY e.id ASC"
	if limit, bounded := overFetch(q.Limit); bounded {
		args = append(args, limit)
		query += fmt.Sprintf(" LIMIT $%d", len(args))
	}
	return selection{sql: query, args: args, order: "id ASC"}
}

// byteCutSQL wraps sel so it returns only the leading rows whose row text, summed in sel's
// order, fits within the budget bound at $budgetArg, selecting columns and then whether another
// row followed each one before the cut. For the last row returned, that says whether the cut
// discarded anything.
//
// The running sum is taken over rows already limited to sel's own over-fetch (the caller's
// limit plus one), so it never scans past them, and it is filtered one level up because a WHERE
// clause is applied before the window functions at its own level.
func byteCutSQL(columns string, sel selection, budgetArg int) string {
	return fmt.Sprintf(`
		SELECT %[1]s, more FROM (
			SELECT l.*,
			       sum(octet_length(l::text))
			         OVER (ORDER BY %[2]s ROWS UNBOUNDED PRECEDING) AS running,
			       lead(true, 1, false) OVER (ORDER BY %[2]s) AS more
			FROM (%[3]s) l
		) w
		WHERE running <= $%[4]d
		ORDER BY %[2]s`, columns, sel.order, sel.sql, budgetArg)
}

// anyRows reports whether sel selects any row. A byte cut that returns no rows cannot tell
// from its rows whether there were any to discard, so it asks.
func (s *Store) anyRows(ctx context.Context, sel selection) (bool, error) {
	var exists bool
	err := s.pool.QueryRow(ctx, "SELECT EXISTS ("+sel.sql+")", sel.args...).Scan(&exists)
	if err != nil {
		return false, fmt.Errorf("check whether a cut read had rows: %w", err)
	}
	return exists, nil
}

// AppendEvent stores an event and folds it into the projection in one transaction.
//
// A duplicate idempotency key is not an error: CI retries are normal traffic, so the
// stored event is returned with created=false. That holds whether the retry arrives
// after the first attempt finished — the lookup below finds it — or while it is still
// in flight, which lands on the unique index instead; see recoverConcurrentDuplicate.
//
// lock_timeout is set as the transaction's FIRST statement, before anything that can
// wait. It used to be set inside lockProjectionWrite, which an append only reaches after
// insertEvent — so the INSERT's wait on uq_event_idem's speculative-insertion lock was
// unbounded, held only by the winning transaction's own lifetime. That is precisely the
// pinned-pool-connection hazard AppendLockTimeout exists to prevent, on the one statement
// it did not cover.
func (s *Store) AppendEvent(
	ctx context.Context, ev core.Event,
) (stored core.Event, created bool, err error) {
	ctx, span := tracer().Start(ctx, "AppendEvent")
	defer span.End()

	// submitted is the event as the caller sent it. The deferred record must NOT read the
	// ev that prepareEvent returns: on a validation failure prepareEvent returns a zero
	// core.Event, so tenant, id, type, source, subject and ref would all be blank on
	// exactly the error class these attributes exist to make debuggable.
	submitted := ev
	out := appendOutcome{dedupePath: dedupeNone}
	defer func() {
		out.created = created
		out.errKind = errKindOf(err)
		span.SetAttributes(out.attributes(submitted)...)
		s.appends.Add(ctx, 1, metric.WithAttributeSet(out.metricAttributes()))
		if err != nil {
			span.RecordError(err)
			span.SetStatus(codes.Error, out.errKind)
		}
	}()

	ev, err = prepareEvent(ev, s.maxAttributeBytes)
	if err != nil {
		return core.Event{}, false, err
	}

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return core.Event{}, false, fmt.Errorf("begin transaction: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	if err = setLockTimeout(ctx, tx, AppendLockTimeout, "append"); err != nil {
		return core.Event{}, false, err
	}

	existing, found, err := findExistingDuplicate(ctx, tx, ev)
	if err != nil {
		return core.Event{}, false, err
	}
	if found {
		out.dedupePath = dedupeLookup
		return existing, false, tx.Commit(ctx)
	}
	return s.writeEvent(ctx, tx, ev, &out)
}

// prepareEvent validates the event and normalises the fields the schema constrains.
//
// A nil Attributes map and an empty one are semantically identical — both mean "no
// attributes" — but the attributes column is JSONB NOT NULL, and Postgres's DEFAULT only
// fires when a column is omitted from the INSERT list, never when NULL is bound
// explicitly, so a nil map reaches the constraint and fails. The NOT NULL constraint lives
// here, so this is where the contract is owned: normalising once at the store boundary
// covers every caller (ingress, CloudEvents mapping, a future egress seed, any other
// producer) instead of repairing them one at a time at their call sites.
// A store opened WithMaxEventAttributeBytes also refuses attributes over that size here,
// after validation, so the refusal is an invalid event like any other.
func prepareEvent(ev core.Event, maxAttributeBytes int) (core.Event, error) {
	if err := ev.Validate(); err != nil {
		return core.Event{}, err
	}
	if maxAttributeBytes > 0 {
		if err := ev.CheckAttributeSize(maxAttributeBytes); err != nil {
			return core.Event{}, err
		}
	}
	if ev.Attributes == nil {
		ev.Attributes = map[string]any{}
	}
	return ev, nil
}

// findExistingDuplicate returns the event already stored under ev's idempotency key, if
// ev carries one and a stored event holds it. An event with no key can never be a
// duplicate, so it reports found=false without a round trip.
func findExistingDuplicate(
	ctx context.Context, tx pgx.Tx, ev core.Event,
) (core.Event, bool, error) {
	if ev.IdempotencyKey == "" {
		return core.Event{}, false, nil
	}
	existing, found, err := findByIdemKey(ctx, tx, ev.Tenant, ev.IdempotencyKey)
	if err != nil {
		return core.Event{}, false, err
	}
	if found {
		return existing, true, nil
	}
	if afterIdemLookupHook != nil {
		afterIdemLookupHook()
	}
	return core.Event{}, false, nil
}

// writeEvent inserts the event, folds it into the projection and commits, all on tx.
//
// The 23505 arm is the concurrent-retry path: the check in findExistingDuplicate and this
// insert are not atomic, so two in-flight requests carrying one idempotency key both find
// nothing and both insert. The loser lands on the unique index and must be given the
// stored event rather than the constraint violation; see recoverConcurrentDuplicate.
func (s *Store) writeEvent(
	ctx context.Context, tx pgx.Tx, ev core.Event, out *appendOutcome,
) (core.Event, bool, error) {
	if err := insertEvent(ctx, tx, ev); err != nil {
		if isUniqueViolation(err, idemConstraint) {
			return s.recoverConcurrentDuplicate(ctx, tx, ev, out)
		}
		return core.Event{}, false, err
	}
	if err := applyToProjection(ctx, tx, ev, out); err != nil {
		return core.Event{}, false, err
	}
	if err := tx.Commit(ctx); err != nil {
		return core.Event{}, false, fmt.Errorf("commit event %q: %w", ev.ID, err)
	}
	return ev, true, nil
}

// insertEvent writes the immutable event row. An empty idempotency key is bound as NULL:
// uq_event_idem is partial on idempotency_key IS NOT NULL, so binding "" instead would
// make every keyless event collide with every other one.
func insertEvent(ctx context.Context, tx pgx.Tx, ev core.Event) error {
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
		return fmt.Errorf("insert event %q: %w", ev.ID, err)
	}
	return nil
}

// isUniqueViolation reports whether err is the Postgres unique violation raised by the
// named constraint or unique index.
func isUniqueViolation(err error, constraint string) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) &&
		pgErr.Code == uniqueViolation && pgErr.ConstraintName == constraint
}

// isLockTimeout reports whether err is Postgres giving up on a lock wait because
// lock_timeout expired.
func isLockTimeout(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == lockNotAvailable
}

// setLockTimeout bounds how long any later statement in tx may wait for a lock.
//
// set_config(name, value, is_local => true) IS "SET LOCAL": it applies to this transaction
// and reverts when the transaction commits or rolls back — the same property that made
// pg_advisory_xact_lock the right choice over pg_advisory_lock, and the reason no code
// path has to remember to unset it. SET itself accepts no bind parameter, so set_config is
// how the Go constant stays the single source of truth for the value. lock_timeout reads a
// bare number as milliseconds.
//
// The bound covers everything the transaction does after this call and nothing before it,
// which is why both callers issue it as their transaction's first statement rather than
// alongside the advisory locks. The event INSERT waits on uq_event_idem's
// speculative-insertion lock, the row-level FOR UPDATE and the change upsert wait on
// another transaction, and the advisory locks wait on whoever holds them: pinning a pool
// connection on any of them is the same hazard, so the bound has to precede all of them.
func setLockTimeout(ctx context.Context, tx pgx.Tx, d time.Duration, what string) error {
	if _, err := tx.Exec(ctx, `SELECT set_config('lock_timeout', $1, true)`,
		strconv.FormatInt(d.Milliseconds(), 10)); err != nil {
		return fmt.Errorf("bound the %s lock wait at %s: %w", what, d, err)
	}
	return nil
}

// recoverConcurrentDuplicate returns the stored event after this caller lost a race for
// an idempotency key, which is the same result the non-concurrent duplicate path gives.
//
// Two in-flight requests carrying the same (tenant, idempotency_key) — a CI retry firing
// while the first attempt is still running — both find nothing at the check above and
// both insert. Duplicates are normal traffic, not an error condition, so the
// loser must not surface 23505 to its caller.
//
// The lookup runs on the pool, not on tx: Postgres aborts a transaction on any error, so
// no further statement can run on tx. That is also why the lookup is guaranteed to find
// the row — the unique index made the losing INSERT wait on the winning transaction and
// raise 23505 only once it had committed, so the winner's row is visible to the fresh
// snapshot this read takes.
func (s *Store) recoverConcurrentDuplicate(
	ctx context.Context, tx pgx.Tx, ev core.Event, out *appendOutcome,
) (core.Event, bool, error) {
	_ = tx.Rollback(ctx)
	out.dedupePath = dedupeUniqueViolation

	existing, found, err := findByIdemKey(ctx, s.pool, ev.Tenant, ev.IdempotencyKey)
	if err != nil {
		return core.Event{}, false, err
	}
	if !found {
		return core.Event{}, false, fmt.Errorf(
			"event %q: idempotency key %q/%q collided with a concurrent write but no "+
				"stored event carries it; retry the append",
			ev.ID, ev.Tenant, ev.IdempotencyKey)
	}
	return existing, false, nil
}

// findByIdemKey returns the previously stored event for (tenant, idempotencyKey), if any.
func findByIdemKey(
	ctx context.Context, q querier, tenantID, idempotencyKey string,
) (core.Event, bool, error) {
	var (
		ev   core.Event
		idem *string
	)
	err := q.QueryRow(ctx, `
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

// afterIdemLookupHook is nil in production. Tests set it to widen the window between
// the idempotency-key lookup and the event insert, making the concurrent-duplicate race
// deterministic instead of dependent on machine timing.
var afterIdemLookupHook func()

// afterDeleteHook is nil in production. Tests set it to hold a rebuild open between its
// DELETE and its re-insert, which is the window an append must not be able to enter.
var afterDeleteHook func()

// applyToProjection folds one event onto its change, under the advisory locks that make
// the read-modify-write a critical section.
func applyToProjection(ctx context.Context, tx pgx.Tx, ev core.Event, out *appendOutcome) error {
	ref := core.RefFor(ev)
	if err := lockProjectionWrite(ctx, tx, ev.Tenant, ref, out); err != nil {
		return err
	}
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
	folded := core.Fold(prev, ev)
	out.foldStatus = folded.Status
	out.foldClosed = folded.End != nil
	// ev.ID sorting below the change's current watermark is exactly the condition under
	// which a rebuild diverges from the live projection — the hazard RebuildProjection
	// documents and defers fixing. Recording it turns a known-possible, never-measured
	// risk into a rate that can be graphed, which is what decides whether fixing it is
	// worth doing.
	out.outOfOrder = found && ev.ID < cur.LastEventID
	return upsertChange(ctx, tx, folded)
}

// lockProjectionWrite takes, in one round trip, the two advisory locks an append needs
// before it may read and rewrite a change row.
//
// The per-ref exclusive lock is what makes the fold correct. SELECT ... FOR UPDATE can
// only lock rows already visible to the transaction's snapshot, so when two transactions
// append the first events for a NEW ref they both see nothing, both compute Fold(nil, ev)
// and both run INSERT ... ON CONFLICT DO UPDATE; the second takes the DO UPDATE branch
// with its own EXCLUDED values, Postgres does not re-run the Go-side fold, and the first
// event's contribution is overwritten wholesale with no error raised. Locking the ref's
// *identity* rather than a row that may not exist yet removes that whole class of bug.
//
// The alternative considered and rejected was INSERT ... ON CONFLICT DO NOTHING followed
// by a re-SELECT ... FOR UPDATE: it closes the same race, but it costs a second round trip
// on every append and leaves the correctness resting on a retry loop that has to be right
// rather than on a lock that cannot be wrong. An advisory lock on the ref's identity is
// one statement and needs no retry.
//
// The tenant-scoped SHARED lock is the other half of RebuildProjection's exclusion. A
// rebuild deletes and re-inserts every row a tenant has, including refs no appender
// currently holds, so per-ref locks alone cannot keep the two apart: a rebuild would
// take no lock any appender waits on. Appenders share this lock with each other, so
// two appends to different refs still never contend on it; only a rebuild, which takes
// it exclusively, waits for them and makes them wait for it.
//
// pg_advisory_xact_lock, rather than pg_advisory_lock, matches Migrate and for the same
// reason: Postgres releases it when the transaction commits or rolls back, so there is
// no manual unlock call a cancelled context or a swallowed error can skip, leaving the
// lock held by a connection quietly returned to the pool.
//
// hashtext is 32-bit, so two unrelated refs can collide and briefly serialise against
// each other. That costs throughput, never correctness.
//
// The wait here is bounded by AppendLockTimeout — a rebuild holds the tenant lock
// exclusively for its whole duration, and an unbounded wait behind it pins a pool
// connection that other tenants need — but this function does not set that bound.
// AppendEvent does, as its transaction's first statement, so it covers the event INSERT
// as well as these locks. See AppendLockTimeout for the reasoning.
func lockProjectionWrite(
	ctx context.Context, tx pgx.Tx, tenantID, ref string, out *appendOutcome,
) error {
	start := time.Now()
	_, err := tx.Exec(ctx, `
		SELECT pg_advisory_xact_lock_shared(hashtext($1)),
		       pg_advisory_xact_lock(hashtext($1 || '/' || $2))`,
		tenantID, ref)
	out.lockWait = time.Since(start)
	if err != nil {
		if isLockTimeout(err) {
			out.lockTimeout = true
			return fmt.Errorf(
				"gave up after %s waiting for the projection lock on %q/%q: %w; the "+
					"usual cause is a projection rebuild for this tenant, which holds "+
					"the lock for the whole of its run, but any long-held transaction "+
					"on this tenant or ref will do it — nothing was written, so retry "+
					"the append",
				AppendLockTimeout, tenantID, ref, err)
		}
		return fmt.Errorf(
			"take projection lock for %q/%q: %w; nothing was written, so the append "+
				"can be retried", tenantID, ref, err)
	}
	return nil
}

// lockRebuildWrite takes the tenant-scoped advisory lock exclusively, excluding every
// append for that tenant — see lockProjectionWrite for why the granularity is the tenant
// and not the ref.
//
// The wait is bounded by RebuildLockTimeout so a rebuild that cannot get the lock promptly
// gives up and says so, rather than queueing behind a stream of appends while holding a
// connection of its own.
//
// Setting that bound is still the FIRST thing the rebuild transaction does, because this
// call is RebuildProjection's first statement after Begin — the property AppendEvent had
// to hoist setLockTimeout out of lockProjectionWrite to obtain, and which a rebuild
// already had. Nothing may be added to RebuildProjection above this call.
func lockRebuildWrite(ctx context.Context, tx pgx.Tx, tenantID string) error {
	if err := setLockTimeout(ctx, tx, RebuildLockTimeout, "rebuild"); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx,
		`SELECT pg_advisory_xact_lock(hashtext($1))`, tenantID); err != nil {
		if isLockTimeout(err) {
			return fmt.Errorf(
				"gave up after %s waiting for the rebuild lock on tenant %q: %w; appends "+
					"for this tenant are holding it, so nothing was written — retry when "+
					"the tenant's write traffic is quieter",
				RebuildLockTimeout, tenantID, err)
		}
		return fmt.Errorf(
			"take rebuild lock for tenant %q: %w; nothing was written, so the rebuild "+
				"can be retried", tenantID, err)
	}
	return nil
}

// lockAndLoadChange reads a change FOR UPDATE and reports found=false when the row does
// not exist yet.
//
// The advisory lock taken in lockProjectionWrite is what serialises the fold; FOR UPDATE
// alone cannot, because it can only lock a row the transaction can already see. It is
// kept as the row-level guard against a writer that has not taken the advisory lock —
// that lock is a convention this package must keep, not something the schema enforces.
func lockAndLoadChange(
	ctx context.Context, tx pgx.Tx, tenantID, ref string,
) (core.Change, bool, error) {
	var (
		c         core.Change
		startedAt *time.Time
		endAt     *time.Time
		st        string
	)
	err := tx.QueryRow(ctx, `
		SELECT tenant, ref, subject, type, title, actor, start_at, started_at,
		       first_seen_at, end_at, status, attributes, last_event_id
		FROM change WHERE tenant = $1 AND ref = $2 FOR UPDATE`,
		tenantID, ref,
	).Scan(&c.Tenant, &c.Ref, &c.Subject, &c.Type, &c.Title, &c.Actor,
		&c.Start, &startedAt, &c.FirstSeenAt, &endAt, &st, &c.Attributes, &c.LastEventID)
	if errors.Is(err, pgx.ErrNoRows) {
		return core.Change{}, false, nil
	}
	if err != nil {
		return core.Change{}, false, fmt.Errorf("lock change %q/%q: %w", tenantID, ref, err)
	}
	c.StartedAt = startedAt
	c.End = endAt
	c.Status = core.Status(st)
	return c, true, nil
}

// upsertChange writes a folded change. Both derived columns are absent from the column
// list, because Postgres generates them and rejects any value an INSERT supplies for one:
// end_effective from end_at (migration 0002), and start_at from
// COALESCE(started_at, first_seen_at) (migration 0003). What this writes is the facts the
// domain owns — started_at, first_seen_at, end_at — so there is exactly one writer of each
// derived value and no second writer to drift from.
//
// None of the three timestamps is normalised to UTC on the way in. TIMESTAMPTZ stores an
// absolute instant, so a Go value's location never reaches the column and a .UTC() here
// would be cosmetic. Applying it to one half of the start fact and not the other spelled
// the two in adjacent lines as though the difference meant something.
func upsertChange(ctx context.Context, tx pgx.Tx, c core.Change) error {
	_, err := tx.Exec(ctx, `
		INSERT INTO change (tenant, ref, subject, type, title, actor, started_at,
		                    first_seen_at, end_at, status, attributes, last_event_id)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12)
		ON CONFLICT (tenant, ref) DO UPDATE SET
		  subject=EXCLUDED.subject, type=EXCLUDED.type, title=EXCLUDED.title,
		  actor=EXCLUDED.actor, started_at=EXCLUDED.started_at,
		  first_seen_at=EXCLUDED.first_seen_at, end_at=EXCLUDED.end_at,
		  status=EXCLUDED.status, attributes=EXCLUDED.attributes,
		  last_event_id=EXCLUDED.last_event_id`,
		c.Tenant, c.Ref, c.Subject, c.Type, c.Title, c.Actor,
		c.StartedAt, c.FirstSeenAt, c.End, string(c.Status), c.Attributes, c.LastEventID,
	)
	if err != nil {
		return fmt.Errorf("upsert change %q/%q: %w", c.Tenant, c.Ref, err)
	}
	return nil
}

// ListEvents selects raw events for a tenant, optionally scoped to one change's ref,
// ordered by id ascending (which is chronological, since ids are ULIDs). It also reports
// whether more rows matched than q.Limit allowed through; the over-fetch that detects that
// is invisible to the caller, so q.Limit still means what it says.
//
// q.Ref is a projection ref, matched the way core.RefFor forms one: a correlated
// event's correlation_ref, or an uncorrelated event's own id. Filtering on
// correlation_ref alone made listing a point change's events always return nothing,
// even though that change is in every query result the egress read side serves. The id
// arm is restricted to rows with no correlation_ref so it can only ever match the point
// that owns the ref, never an unrelated event whose id happens to equal some other
// change's correlation_ref.
func (s *Store) ListEvents(
	ctx context.Context, q EventQuery,
) (evs []core.Event, truncated bool, err error) {
	ctx, span := tracer().Start(ctx, "ListEvents")
	defer span.End()
	// Recorded from a defer, not from the success path, so a query that fails still says
	// which tenant asked for what. The limit is known at entry; rows and truncated read
	// from the named results, which on every error path are the zero values this returns.
	defer func() {
		span.SetAttributes(
			attribute.String("horopter.tenant", q.Tenant),
			attribute.Int("horopter.query.limit", q.Limit),
			attribute.Int("horopter.query.rows", len(evs)),
			attribute.Bool("horopter.query.truncated", truncated),
		)
		if err != nil {
			span.RecordError(err)
			span.SetStatus(codes.Error, "")
		}
	}()

	sel := eventSelection(q)
	if q.MaxRowTextBytes > 0 {
		return s.listEventsWithinBytes(ctx, q, sel)
	}
	rows, err := s.pool.Query(ctx,
		"SELECT "+eventColumns+" FROM ("+sel.sql+") l ORDER BY "+sel.order, sel.args...)
	if err != nil {
		return nil, false, fmt.Errorf("list events for tenant %q: %w", q.Tenant, err)
	}
	defer rows.Close()

	evs, err = scanEvents(rows, q.Tenant)
	if err != nil {
		return nil, false, err
	}
	evs, truncated = trimToLimit(evs, q.Limit)
	return evs, truncated, nil
}

// listEventsWithinBytes is ListEvents with q.MaxRowTextBytes applied.
func (s *Store) listEventsWithinBytes(
	ctx context.Context, q EventQuery, sel selection,
) ([]core.Event, bool, error) {
	args := append(slices.Clip(sel.args), q.MaxRowTextBytes)
	rows, err := s.pool.Query(ctx, byteCutSQL(eventColumns, sel, len(args)), args...)
	if err != nil {
		return nil, false, fmt.Errorf("list events within %d bytes for tenant %q: %w",
			q.MaxRowTextBytes, q.Tenant, err)
	}
	defer rows.Close()

	var more bool
	evs, err := scanEvents(rows, q.Tenant, &more)
	if err != nil {
		return nil, false, err
	}
	if len(evs) == 0 {
		cut, err := s.anyRows(ctx, sel)
		return nil, cut, err
	}
	evs, truncated := trimToLimit(evs, q.Limit)
	return evs, truncated || more, nil
}

// scanEvents reads every event row, naming tenantID in any error so a failure identifies
// the query it came from. Extra destinations are scanned after the row's own columns.
func scanEvents(rows pgx.Rows, tenantID string, extra ...any) ([]core.Event, error) {
	var evs []core.Event
	for rows.Next() {
		var (
			ev   core.Event
			idem *string
		)
		dest := []any{&ev.ID, &ev.Tenant, &idem, &ev.SchemaVersion, &ev.Source, &ev.Type,
			&ev.Subject, &ev.Actor, &ev.Title, &ev.OccurredAt, &ev.ReceivedAt,
			&ev.CorrelationRef, &ev.Attributes}
		if err := rows.Scan(append(dest, extra...)...); err != nil {
			return nil, fmt.Errorf("scan event row for tenant %q: %w", tenantID, err)
		}
		if idem != nil {
			ev.IdempotencyKey = *idem
		}
		evs = append(evs, ev)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate events for tenant %q: %w", tenantID, err)
	}
	return evs, nil
}

// QueryChanges returns changes overlapping [q.From, q.To], and reports whether more rows
// matched than q.Limit allowed through.
//
// The predicate is overlap, not start-containment. Filtering on start alone would hide
// a change that began before the window and is still running — precisely the change
// someone most needs to see when zooming into a spike.
//
// The ordering is start_at ASC unconditionally, so truncation discards the most recent
// rows. That is why the truncated flag matters rather than being a nicety: a caller
// otherwise cannot tell a result set of exactly q.Limit rows from one that dropped the
// changes it most needed. The over-fetch that detects it is invisible to the caller —
// q.Limit still means what it says.
func (s *Store) QueryChanges(
	ctx context.Context, q ChangeQuery,
) (out []core.Change, truncated bool, err error) {
	ctx, span := tracer().Start(ctx, "QueryChanges")
	defer span.End()
	// Recorded from a defer, not from the success path, so a query that fails still says
	// which tenant asked for what over how wide a window — the two dimensions a slow or
	// failing dashboard query is diagnosed by. Limit and window are known at entry; rows
	// and truncated read from the named results, which on every error path are the zero
	// values this returns.
	defer func() {
		span.SetAttributes(
			attribute.String("horopter.tenant", q.Tenant),
			attribute.Int("horopter.query.limit", q.Limit),
			attribute.Int("horopter.query.rows", len(out)),
			attribute.Bool("horopter.query.truncated", truncated),
			attribute.Int64("horopter.query.window_ms", q.To.Sub(q.From).Milliseconds()),
		)
		if err != nil {
			span.RecordError(err)
			span.SetStatus(codes.Error, "")
		}
	}()

	sel := changeSelection(q)
	if q.MaxRowTextBytes > 0 {
		return s.queryChangesWithinBytes(ctx, q, sel)
	}
	rows, err := s.pool.Query(ctx,
		"SELECT "+changeColumns+" FROM ("+sel.sql+") l ORDER BY "+sel.order, sel.args...)
	if err != nil {
		return nil, false, fmt.Errorf("query changes for tenant %q: %w", q.Tenant, err)
	}
	defer rows.Close()

	out, err = scanChanges(rows)
	if err != nil {
		return nil, false, err
	}
	out, truncated = trimToLimit(out, q.Limit)
	return out, truncated, nil
}

// queryChangesWithinBytes is QueryChanges with q.MaxRowTextBytes applied.
func (s *Store) queryChangesWithinBytes(
	ctx context.Context, q ChangeQuery, sel selection,
) ([]core.Change, bool, error) {
	args := append(slices.Clip(sel.args), q.MaxRowTextBytes)
	rows, err := s.pool.Query(ctx, byteCutSQL(changeColumns, sel, len(args)), args...)
	if err != nil {
		return nil, false, fmt.Errorf("query changes within %d bytes for tenant %q: %w",
			q.MaxRowTextBytes, q.Tenant, err)
	}
	defer rows.Close()

	var more bool
	out, err := scanChanges(rows, &more)
	if err != nil {
		return nil, false, err
	}
	if len(out) == 0 {
		cut, err := s.anyRows(ctx, sel)
		return nil, cut, err
	}
	out, truncated := trimToLimit(out, q.Limit)
	return out, truncated || more, nil
}

// appendChangeFilters adds the optional equality filters to the query and returns the
// extended argument list. The placeholders are numbered from the argument list's own
// length, so the two must grow together — which is why they are built in one place.
func appendChangeFilters(sb *strings.Builder, args []any, q ChangeQuery) []any {
	if q.Subject != "" {
		args = append(args, q.Subject)
		fmt.Fprintf(sb, " AND subject = $%d", len(args))
	}
	if q.Type != "" {
		args = append(args, q.Type)
		fmt.Fprintf(sb, " AND type = $%d", len(args))
	}
	if q.Status != "" {
		args = append(args, q.Status)
		fmt.Fprintf(sb, " AND status = $%d", len(args))
	}
	return args
}

// scanChanges reads every change row. start_at is read as well as the two facts it is
// generated from, so a loaded Change carries the same start the database holds rather than
// one recomputed from them. Extra destinations are scanned after the row's own columns.
func scanChanges(rows pgx.Rows, extra ...any) ([]core.Change, error) {
	var out []core.Change
	for rows.Next() {
		var (
			c         core.Change
			startedAt *time.Time
			endAt     *time.Time
			st        string
		)
		dest := []any{&c.Tenant, &c.Ref, &c.Subject, &c.Type, &c.Title,
			&c.Actor, &c.Start, &startedAt, &c.FirstSeenAt, &endAt, &st,
			&c.Attributes, &c.LastEventID}
		if err := rows.Scan(append(dest, extra...)...); err != nil {
			return nil, fmt.Errorf("scan change row: %w", err)
		}
		c.StartedAt = startedAt
		c.End = endAt
		c.Status = core.Status(st)
		out = append(out, c)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate change rows: %w", err)
	}
	return out, nil
}

// EstimateChanges measures the rows QueryChanges would fetch for q without fetching them: the
// same tenant, window, filters, order and over-fetch limit. q.MaxRowTextBytes is ignored; an
// estimate is what decides whether to set it.
func (s *Store) EstimateChanges(ctx context.Context, q ChangeQuery) (ReadEstimate, error) {
	return s.estimate(ctx, "EstimateChanges", q.Tenant, changeSelection(q))
}

// EstimateEvents measures the rows ListEvents would fetch for q, as EstimateChanges does.
func (s *Store) EstimateEvents(ctx context.Context, q EventQuery) (ReadEstimate, error) {
	return s.estimate(ctx, "EstimateEvents", q.Tenant, eventSelection(q))
}

// estimate sums the row text of every row sel selects and takes the first row's by sel's
// order, under a span named operation.
func (s *Store) estimate(
	ctx context.Context, operation, tenantID string, sel selection,
) (est ReadEstimate, err error) {
	ctx, span := tracer().Start(ctx, operation)
	defer span.End()
	defer func() {
		span.SetAttributes(attribute.String("horopter.tenant", tenantID))
		if err != nil {
			span.RecordError(err)
			span.SetStatus(codes.Error, "")
		}
	}()

	query := fmt.Sprintf(`
		SELECT count(*), coalesce(sum(b), 0), coalesce((array_agg(b ORDER BY %s))[1], 0)
		FROM (SELECT octet_length(l::text) AS b, l.* FROM (%s) l) s`, sel.order, sel.sql)
	err = s.pool.QueryRow(ctx, query, sel.args...).Scan(
		&est.Rows, &est.TotalBytes, &est.FirstRowBytes)
	if err != nil {
		return ReadEstimate{}, fmt.Errorf("estimate a read for tenant %q: %w", tenantID, err)
	}
	return est, nil
}

// RebuildProjection discards a tenant's projection and replays its event log in ULID
// order, holding the tenant's advisory lock exclusively so no append can interleave
// with the delete-and-reinsert.
//
// Without that lock an appender blocked on a row this transaction deleted re-checks,
// finds nothing, folds from nil and overwrites the rebuilt row — the same lost-update
// mechanism the per-ref lock exists to stop. A per-ref lock is the wrong granularity
// here because a rebuild touches refs no appender is holding, including refs that do
// not exist yet; see lockProjectionWrite for how the two locks compose.
//
// What it reproduces is the id-ordered fold, which is not always the projection
// production holds. Live appends fold in COMMIT order — the order in which append
// transactions take the per-ref advisory lock — and a rebuild folds in id order. The two
// diverge for ANY pair of same-ref events whose commit order differs from their id order.
// No sub-millisecond coincidence is needed for that, because an event's id is minted from
// s.clock() on the receiving ingress replica BEFORE its append transaction starts, and
// nothing afterwards keeps the two orders aligned:
//
//   - Across replicas the divergence window is the CLOCK SKEW between them, not one
//     millisecond. A deployment.completed handled by replica B can carry a lower id than a
//     deployment.started handled by replica A that arrived earlier, and then folds second.
//   - Within a single replica the per-ref advisory lock itself reorders: a request that
//     minted a lower id can lose the race for the lock while one holding a higher id folds
//     first. The window is everything between minting the id and taking that lock — Begin,
//     the idempotency lookup, the event INSERT, and the lock wait, which AppendLockTimeout
//     bounds at seconds.
//   - Only then, as the narrowest of the three, does entropy matter: core.NewULID draws
//     from crypto/rand rather than ulid.MonotonicEntropy, so two ids minted inside one
//     millisecond do not even tie-break by generation order.
//
// A rebuild can therefore produce a different Subject, Type, Title, Actor, attribute set,
// LastEventID, End or Status than the one live folding produced. The first six are
// assigned unconditionally, so their rule is "the last event folded wins". End and Status
// follow a narrower one — core.Fold guards both on End being nil, so once a change has
// closed only a terminal event moves them: "the last TERMINAL event folded wins".
// Reordering two terminal events swaps the recorded outcome; a late annotation cannot
// reopen a change that already closed. Start, StartedAt and FirstSeenAt are NOT in that set:
// both start facts are minima (migration 0003, core.Fold), so they are commutative and the
// two orders agree on them. Every event is still folded in, and the rebuild is still
// deterministic — replaying the same log again gives the same answer — but "reproduces the
// projection exactly" is only true up to that ordering.
//
// This is reachable by following the documentation: --rebuild-tenant is the README's
// stated remedy for migration 0003, so an operator running it can flip a change's recorded
// outcome from completed to failed, or back. The remedy, deferred to v0.2 and deliberately
// not attempted here, is to make every field's rule commutative or to detect ev.ID <
// cur.LastEventID during an append and re-fold that ref from the log. Until then the
// README's rebuild section carries the same caveat, where an operator will actually meet
// it.
//
// Operational exposure, which v0.1 does not remove: the exclusive lock blocks every
// append for this tenant for the rebuild's whole duration. Blocked appends fail after
// AppendLockTimeout with a retryable error rather than pinning pool connections, so the
// bound on an event's delay is the producer's own retry patience. There is no batching,
// no progress reporting and no online rebuild.
func (s *Store) RebuildProjection(ctx context.Context, tenantID string) (err error) {
	ctx, span := tracer().Start(ctx, "RebuildProjection")
	defer span.End()

	// Recorded from a defer, not after the replay, so the failure an operator actually
	// meets is covered: a rebuild that gives up in lockRebuildWrite on RebuildLockTimeout
	// used to produce a span saying neither which tenant it was for nor how long it
	// waited. The counters stay zero on that path, which is the truth — nothing was
	// replayed.
	var (
		lockWait time.Duration
		events   int
		refs     int
	)
	defer func() {
		span.SetAttributes(
			attribute.String("horopter.rebuild.tenant", tenantID),
			attribute.Int("horopter.rebuild.events", events),
			attribute.Int("horopter.rebuild.refs", refs),
			attribute.Int64("horopter.rebuild.lock_wait_ms", lockWait.Milliseconds()),
		)
		if err != nil {
			span.RecordError(err)
			span.SetStatus(codes.Error, "")
		}
	}()

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin rebuild transaction: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	// lockWait is assigned before the error is checked, so a lock that timed out reports
	// the wait that ended in the timeout rather than zero.
	lockStart := time.Now()
	lockErr := lockRebuildWrite(ctx, tx, tenantID)
	lockWait = time.Since(lockStart)
	if lockErr != nil {
		return lockErr
	}
	if err := clearProjection(ctx, tx, tenantID); err != nil {
		return err
	}

	events, refs, err = replayProjection(ctx, tx, tenantID)
	if err != nil {
		return err
	}

	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit rebuild for tenant %q: %w", tenantID, err)
	}
	return nil
}

// replayProjection folds a tenant's whole event log in id order and writes the result,
// returning the number of events replayed and the number of refs they folded into.
//
// Kept out of RebuildProjection so that function stays legible while it carries the lock,
// the clear, the span and the commit — and so it has complexity headroom, which it does
// not otherwise have.
// The results are NAMED because they are two bare ints in a row: nothing catches a
// transposition at either end, and TestRebuildRecordsItsShapeOnASpan seeds two events
// onto one ref specifically so the two values differ and a swap is visible.
func replayProjection(
	ctx context.Context, tx pgx.Tx, tenantID string,
) (events, refs int, err error) {
	acc, events, err := foldEventLog(ctx, tx, tenantID)
	if err != nil {
		return 0, 0, err
	}
	if err := writeProjection(ctx, tx, acc); err != nil {
		return 0, 0, err
	}
	return events, len(acc), nil
}

// clearProjection discards a tenant's projection inside the rebuild transaction. The hook
// is what lets a test hold the rebuild open in the window an append must not enter.
func clearProjection(ctx context.Context, tx pgx.Tx, tenantID string) error {
	if _, err := tx.Exec(ctx, `DELETE FROM change WHERE tenant = $1`, tenantID); err != nil {
		return fmt.Errorf("clear projection for tenant %q: %w", tenantID, err)
	}
	if afterDeleteHook != nil {
		afterDeleteHook()
	}
	return nil
}

// foldEventLog replays a tenant's event log in ULID order and returns the projection it
// implies, keyed by ref, together with the number of events it read to build it.
//
// Folding inside the scan loop, rather than materialising the whole log into a slice
// first, keeps peak memory proportional to the number of distinct refs rather than the
// total number of events replayed.
//
// The accumulator passes a nil *core.Change for the first event of each ref, matching how
// AppendEvent applies events incrementally; core.Fold is written so both spellings
// converge.
func foldEventLog(
	ctx context.Context, tx pgx.Tx, tenantID string,
) (map[string]core.Change, int, error) {
	rows, err := tx.Query(ctx, `
		SELECT id, tenant, schema_version, source, type, subject, actor, title,
		       occurred_at, received_at, correlation_ref, attributes
		FROM event WHERE tenant = $1 ORDER BY id ASC`, tenantID)
	if err != nil {
		return nil, 0, fmt.Errorf("read event log for tenant %q: %w", tenantID, err)
	}
	defer rows.Close()

	acc := map[string]core.Change{}
	events := 0
	for rows.Next() {
		var ev core.Event
		if err := rows.Scan(&ev.ID, &ev.Tenant, &ev.SchemaVersion, &ev.Source, &ev.Type,
			&ev.Subject, &ev.Actor, &ev.Title, &ev.OccurredAt, &ev.ReceivedAt,
			&ev.CorrelationRef, &ev.Attributes); err != nil {
			return nil, 0, fmt.Errorf("scan event row for tenant %q: %w", tenantID, err)
		}
		events++
		ref := core.RefFor(ev)
		var prev *core.Change
		if c, ok := acc[ref]; ok {
			prev = &c
		}
		acc[ref] = core.Fold(prev, ev)
	}
	if err := rows.Err(); err != nil {
		return nil, 0, fmt.Errorf("iterate event log for tenant %q: %w", tenantID, err)
	}
	return acc, events, nil
}

// writeProjection inserts every rebuilt change.
func writeProjection(ctx context.Context, tx pgx.Tx, acc map[string]core.Change) error {
	for _, c := range acc {
		if err := upsertChange(ctx, tx, c); err != nil {
			return err
		}
	}
	return nil
}
