// Package egress is the read-side daemon: it serves queries and renders them through
// vendor adapters.
//
// It contains no write code by design: no code here calls AppendEvent or
// RebuildProjection, and Routes exposes no path that reaches them. That is a
// discipline this package's code observes, not a privilege boundary — Server holds a
// full *postgres.Store, which exposes those same write methods, so a compromise of the
// egress process itself still has a write-capable Store sitting in memory. What
// splitting into two binaries buys is that a bug specific to the write path is only
// reachable through ingress's routes, and a bug specific to the read path only through
// egress's.
package egress

import (
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"

	"go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"
	"horopter.dev/horopter/internal/adapter/grafana"
	"horopter.dev/horopter/internal/adapter/otlp"
	"horopter.dev/horopter/internal/admission"
	"horopter.dev/horopter/internal/auth"
	"horopter.dev/horopter/internal/core"
	"horopter.dev/horopter/internal/logging"
	"horopter.dev/horopter/internal/store/postgres"
)

// MaxLimit bounds every query's result set, by default and as a hard cap on any
// caller-supplied "limit" parameter. A credential's read capability bounds WHOSE data a
// caller can reach; MaxLimit bounds HOW MUCH of it a single request can pull, so a valid
// reader still cannot exhaust the database or exfiltrate an entire tenant's event log or
// change history in one request.
const MaxLimit = 1000

// TruncatedHeader is set to "true" on a response whose result set was cut short by the
// limit, and is absent otherwise. Without it a caller cannot tell a result of exactly
// MaxLimit rows from one where more exist — and since the ordering is start_at ASC,
// truncation discards the most RECENT rows, which during an incident are exactly the ones
// that matter. Silently returning the cap as if it were the whole answer is the failure
// this closes.
//
// A header rather than a flag in the body: two of the four read endpoints render
// vendor-owned response shapes (Grafana annotations, OTLP spans) that cannot carry a
// Horopter envelope, so a header is the only signal that works uniformly across all four
// without breaking any of them.
const TruncatedHeader = "X-Horopter-Truncated"

// Server is the egress HTTP surface.
type Server struct {
	store   *postgres.Store
	clock   func() time.Time
	grafana *grafana.Renderer
	otlp    *otlp.Renderer
	limits  Limits
	limiter *admission.Limiter
	memory  *admission.BytePool
	metrics readMetrics
	authn   *auth.Authenticator
}

// New returns an egress Server admitting reads against limits and authenticating every
// non-probe route through authn. clock is injected so rendering an open change stays
// deterministic under test.
//
// A nil authenticator is refused here rather than left to callers to check: every read
// route is gated on it, so a Server built without one would either panic on the first
// request or, worse, need every caller to remember the guard itself.
func New(
	s *postgres.Store, clock func() time.Time, limits Limits, authn *auth.Authenticator,
) (*Server, error) {
	if authn == nil {
		return nil, errors.New(
			"egress requires an authenticator; build one with auth.New over a credential " +
				"store — a nil authenticator would panic on the first request rather than " +
				"reject it")
	}
	floor, initial, ceiling := concurrencyBounds(s)
	srv := &Server{
		store: s, clock: clock, grafana: grafana.New(), otlp: otlp.New(),
		limits:  limits,
		limiter: admission.NewLimiter(floor, initial, ceiling),
		memory:  admission.NewBytePool(limits.ReadMemoryBytes),
		authn:   authn,
	}
	srv.metrics = newReadMetrics(srv.limiter, srv.memory)
	return srv, nil
}

// concurrencyBounds derives the read concurrency limit from the store's connection pool: it
// starts at the pool's size, may fall to one read, and may rise to four times the pool, since
// part of each read's time is spent rendering rather than holding a connection. A nil store
// serves only the probes, which pass no gate, so its bounds are never used.
func concurrencyBounds(s *postgres.Store) (floor, initial, ceiling int) {
	if s == nil {
		return 1, 1, 1
	}
	conns := int(s.Pool().Config().MaxConns)
	return 1, conns, 4 * conns
}

// Routes returns the handler for every egress endpoint.
func (s *Server) Routes() http.Handler {
	mux := http.NewServeMux()
	mux.Handle("GET /v1/changes", s.authz(auth.CapRead, s.admit(s.queryChanges)))
	mux.Handle("GET /v1/events", s.authz(auth.CapRead, s.admit(s.listEvents)))
	mux.Handle("GET /adapters/grafana/annotations",
		s.authz(auth.CapRead, s.admit(s.grafanaAnnotations)))
	mux.Handle("GET /adapters/otlp/spans", s.authz(auth.CapRead, s.admit(s.otlpSpans)))
	mux.HandleFunc("GET /healthz", s.healthz)
	mux.HandleFunc("GET /readyz", s.readyz)
	// otelhttp gives every route a server span carrying the http.* semantic
	// conventions. It wraps the mux rather than each handler so a route added later is
	// covered without anyone remembering. With no provider installed it is a no-op.
	// Every route but the probes passes auth then admit, in that order, so a probe answers
	// while reads are being shed, and an unauthenticated request is refused before it can
	// spend a read permit.
	//
	// The operation argument is empty because it has no effect here. Without a
	// WithSpanNameFormatter option, otelhttp's default formatter ignores the operation
	// entirely and names the span from the request's method and route; passing the
	// binary name only made it look load-bearing. The binary name already reaches
	// backends as the service.name resource attribute.
	// WithQuery: a read's filters are what tell "empty because nothing matched" from
	// "empty because the filter was wrong". Logged only once a credential resolves.
	return otelhttp.NewHandler(logging.Requests(mux, logging.WithQuery()), "")
}

// authz gates next on capability c, wrapping admission rather than sitting inside it: an
// unauthenticated request — and the pool connection its credential lookup would take — is
// refused before a read permit is ever spent. See admission.go's admit for the other half.
func (s *Server) authz(c auth.Capability, next http.Handler) http.Handler {
	return s.authn.Require(c)(next)
}

// healthz reports process liveness only. It must not touch the database: a failing
// liveness probe restarts the pod, and restarting will not fix a database outage.
func (s *Server) healthz(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

// readyz reports whether this instance can serve traffic, which requires the database.
// A failing readiness probe removes the pod from the load balancer without killing it.
//
// The probe itself is unauthenticated, like ingress's: an orchestrator holds no credential
// to send. pgx formats a failed connection with the database user, database name, host and
// port, so the error goes to the server log and the body says only that the database is
// unreachable, as readFailed does for a failed read.
func (s *Server) readyz(w http.ResponseWriter, r *http.Request) {
	if err := s.store.Pool().Ping(r.Context()); err != nil {
		slog.ErrorContext(r.Context(), "readiness probe failed", "error", err)
		writeError(w, http.StatusServiceUnavailable, "database unreachable")
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "ready"})
}

// errTenantRequired reports a global credential naming no tenant: there is no default left
// to fall back to.
var errTenantRequired = errors.New(`query parameter "tenant" is required for this credential`)

// errTenantForbidden reports a tenant-scoped credential naming a tenant other than its own.
var errTenantForbidden = errors.New("credential is scoped to a different tenant")

// errTenantNotFound reports a named tenant no row matches. Its text must never include the
// id: a uniform 404 exists precisely so a caller enumerating tenant ids cannot tell an
// unowned tenant from one that never existed.
var errTenantNotFound = errors.New("tenant not found")

// tenantFor resolves the tenant a request may read, from the authenticated principal
// auth.Require attached to its context and the "?tenant=" query parameter — the only place
// a tenant is named now that no route carries a {tenant} path segment.
//
// A tenant-scoped credential may name its own tenant, redundantly, or none at all; naming
// any other is errTenantForbidden. A global credential carries no tenant of its own, so it
// must name one: errTenantRequired when it does not, errTenantNotFound when the named
// tenant does not exist.
//
// A request reaching here without a principal on its context — every route Routes()
// registers passes through authz first, so this should never happen — must not be read as
// a global credential: Principal{}.IsGlobal() is true for the zero value, which would
// otherwise treat a bypassed auth check as a superuser able to name any tenant. Failing
// closed here is the runtime default; the structural test that every route is authz-wrapped
// catches the bypass itself, but that test is not where this guarantee should live.
func (s *Server) tenantFor(r *http.Request) (core.TenantID, error) {
	p, ok := auth.PrincipalFrom(r.Context())
	if !ok {
		return core.TenantID{}, errTenantRequired
	}
	named := r.URL.Query().Get("tenant")
	if !p.IsGlobal() {
		if named != "" && named != p.Tenant.String() {
			return core.TenantID{}, errTenantForbidden
		}
		return p.Tenant, nil
	}
	if named == "" {
		return core.TenantID{}, errTenantRequired
	}
	exists, err := s.store.TenantExists(r.Context(), named)
	if err != nil {
		return core.TenantID{}, fmt.Errorf("check tenant %q exists: %w", named, err)
	}
	if !exists {
		return core.TenantID{}, errTenantNotFound
	}
	return core.TenantIDFromRow(named), nil
}

// writeTenantError answers a failed tenantFor resolution with the status its error names:
// 400 for no tenant named, 403 for one outside the credential's scope, 404 for one that
// does not exist, and 500 — logged, never relayed, as readFailed does — for a store failure
// encountered while checking.
func writeTenantError(w http.ResponseWriter, r *http.Request, err error) {
	switch {
	case errors.Is(err, errTenantRequired):
		writeError(w, http.StatusBadRequest, err.Error())
	case errors.Is(err, errTenantForbidden):
		writeError(w, http.StatusForbidden, err.Error())
	case errors.Is(err, errTenantNotFound):
		writeError(w, http.StatusNotFound, err.Error())
	default:
		slog.ErrorContext(r.Context(), "tenant resolution failed", "error", err,
			"route", r.Pattern)
		writeError(w, http.StatusInternalServerError,
			"could not resolve the requested tenant; the cause has been logged on the server")
	}
}

// rfc3339Param parses an RFC3339 query parameter, naming the expected format on error.
func rfc3339Param(r *http.Request, name string) (time.Time, error) {
	raw := r.URL.Query().Get(name)
	if raw == "" {
		return time.Time{}, fmt.Errorf(
			"query parameter %q is required, as RFC3339, e.g. 2026-09-14T10:00:00Z", name)
	}
	t, err := time.Parse(time.RFC3339, raw)
	if err != nil {
		return time.Time{}, fmt.Errorf(
			"query parameter %q=%q is not RFC3339: %w", name, raw, err)
	}
	return t, nil
}

// minEpochMillis and maxEpochMillis bound a Grafana-style epoch-millisecond parameter.
//
// The ceiling is core.OpenSentinel, the literal migration 0002 generates for an open
// change's end_effective: nothing in the projection can extend past it, so a window
// reaching beyond it can contain nothing a shorter one does not. The floor is the Unix
// epoch, which is where this transport's own zero is.
//
// The bound belongs at the parser, because nothing downstream of it reports the problem.
// Without it any int64 was accepted: ?to=9223372036854775807 became a year-292278994
// time.Time, and pgx encodes timestamptz as microseconds since 2000-01-01 in an int64, so
// that value OVERFLOWS and wraps. Measured against this stack (pgx v5, Postgres 18) it
// binds as 1969-12-31T23:59:59.999Z — a caller asking for everything up to the end of
// time is silently given a window ending before the Unix epoch, and answered 200 with an
// empty list. No error is raised anywhere, so nothing is logged and there is nothing for
// an operator to notice; the caller simply gets a confident wrong answer. A 400 naming
// the parameter is the only place that can be caught.
var (
	minEpochMillis = time.Unix(0, 0).UTC().UnixMilli()
	maxEpochMillis = core.OpenSentinel.UnixMilli()
)

// epochMillisParam parses a Grafana-style epoch-millisecond parameter.
func epochMillisParam(r *http.Request, name string) (time.Time, error) {
	raw := r.URL.Query().Get(name)
	if raw == "" {
		return time.Time{}, fmt.Errorf(
			"query parameter %q is required; Grafana sends it as epoch milliseconds", name)
	}
	ms, err := strconv.ParseInt(raw, 10, 64)
	if err != nil {
		return time.Time{}, fmt.Errorf(
			"query parameter %q=%q is not epoch milliseconds: %w", name, raw, err)
	}
	if ms < minEpochMillis || ms > maxEpochMillis {
		return time.Time{}, fmt.Errorf(
			"query parameter %q=%d is out of range; epoch milliseconds must be between "+
				"%d (%s) and %d (%s)",
			name, ms,
			minEpochMillis, time.UnixMilli(minEpochMillis).UTC().Format(time.RFC3339),
			maxEpochMillis, time.UnixMilli(maxEpochMillis).UTC().Format(time.RFC3339))
	}
	return time.UnixMilli(ms).UTC(), nil
}

// limitParam parses the optional "limit" query parameter. Absent, it defaults to
// MaxLimit; present, it is capped at MaxLimit rather than rejected (a caller asking
// for more than the cap gets the cap, not an error), and rejected only when it is not
// a positive integer.
func limitParam(r *http.Request) (int, error) {
	raw := r.URL.Query().Get("limit")
	if raw == "" {
		return MaxLimit, nil
	}
	n, err := strconv.Atoi(raw)
	if err != nil {
		return 0, fmt.Errorf(
			"query parameter \"limit\"=%q is not a number; expected an integer "+
				"between 1 and %d", raw, MaxLimit)
	}
	if n < 1 {
		return 0, fmt.Errorf(
			"query parameter \"limit\"=%d is out of range; expected an integer "+
				"between 1 and %d", n, MaxLimit)
	}
	if n > MaxLimit {
		return MaxLimit, nil
	}
	return n, nil
}

// window bounds a query in both time and result-set size.
type window struct {
	from, to time.Time
	limit    int
}

// newWindow assembles a validated window, and is the single place from and to are
// checked against each other. Both parsers below build their result through it — RFC3339
// query parameters and Grafana's epoch milliseconds — so a third transport cannot be added
// without inheriting the rule.
//
// An inverted window is the caller's error, not an empty result. The overlap predicate
// is "start_at <= :to AND end_effective >= :from", which for from > to selects the
// changes SPANNING the interval the caller inverted: a non-empty answer bearing no
// relation to the question asked, and indistinguishable from a correct one.
func newWindow(from, to time.Time, limit int) (window, error) {
	if from.After(to) {
		return window{}, fmt.Errorf(
			"query window from=%s is after to=%s; give the range earliest first",
			from.UTC().Format(time.RFC3339), to.UTC().Format(time.RFC3339))
	}
	return window{from: from, to: to, limit: limit}, nil
}

// windowFromRFC3339 reads from/to as RFC3339 and limit as an optional bounded integer,
// matching the neutral API and the OTLP adapter's parameter contract. When the request
// names a ref, from and to are not required: (tenant, ref) is a change's primary key, so a
// ref lookup is already exact, and changeSelection omits the window predicate entirely
// whenever a ref is set — a window alongside one would be parsed for nothing.
func windowFromRFC3339(r *http.Request) (window, error) {
	limit, err := limitParam(r)
	if err != nil {
		return window{}, err
	}
	if r.URL.Query().Get("ref") != "" {
		return window{limit: limit}, nil
	}
	from, err := rfc3339Param(r, "from")
	if err != nil {
		return window{}, err
	}
	to, err := rfc3339Param(r, "to")
	if err != nil {
		return window{}, err
	}
	return newWindow(from, to, limit)
}

// windowFromEpochMillis reads from/to as epoch milliseconds and limit as an optional
// bounded integer: the neutral epoch-millisecond query-parameter contract, and the one
// grafana/simple-json-datasource — an archived Grafana datasource plugin, but the only one
// that speaks this transport — actually drives.
func windowFromEpochMillis(r *http.Request) (window, error) {
	from, err := epochMillisParam(r, "from")
	if err != nil {
		return window{}, err
	}
	to, err := epochMillisParam(r, "to")
	if err != nil {
		return window{}, err
	}
	limit, err := limitParam(r)
	if err != nil {
		return window{}, err
	}
	return newWindow(from, to, limit)
}

// attributeFilters collects the ?attr.<key>=<value> parameters into the map NewChangeQuery
// validates. Everything after the prefix is the key, dots included: attribute keys are
// dotted by convention — deployment.environment.name — so a parser that split on "." would
// truncate every real one.
//
// An empty key survives here rather than being dropped, so that ?attr.= is refused by the
// allowlist like any other unknown key instead of being silently ignored.
func attributeFilters(r *http.Request) map[string]string {
	var out map[string]string
	for name, values := range r.URL.Query() {
		key, ok := strings.CutPrefix(name, "attr.")
		if !ok {
			continue
		}
		if out == nil {
			out = map[string]string{}
		}
		out[key] = values[0]
	}
	return out
}

// changesFor reads the changes win selects: it estimates their size, has admission reserve
// their memory, then queries them. It answers the request itself on any failure, and then
// reports ok false.
//
// The tenant comes from context, not a fresh tenantFor(r) call: admit already resolved and
// validated it before this handler ever ran, and for a global credential that resolution is
// a database round trip a query-building step must not repeat.
func (s *Server) changesFor(
	w http.ResponseWriter, r *http.Request, win window, adapter string, after *postgres.ChangeKey,
) (changes []core.Change, truncated, ok bool) {
	tenantID := tenantFromContext(r.Context())
	trace.SpanFromContext(r.Context()).SetAttributes(
		attribute.String("horopter.tenant", tenantID.String()))
	q, err := postgres.NewChangeQuery(
		tenantID, win.from, win.to, win.limit, attributeFilters(r))
	if err != nil {
		// An unfilterable attribute key is the caller's mistake, not a failed read, so it
		// answers 400 with the store's own message — which names the key and lists what is
		// filterable. readFailed's 500 would report the server broken instead.
		if errors.Is(err, postgres.ErrUnfilterableAttribute) {
			writeError(w, http.StatusBadRequest, err.Error())
			return nil, false, false
		}
		s.readFailed(w, r, adapter, err)
		return nil, false, false
	}
	subjects, err := core.ParseSubjectPatterns(r.URL.Query()["subject"])
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return nil, false, false
	}
	q.Subjects = subjects
	q.Type = r.URL.Query().Get("type")
	q.Status = r.URL.Query().Get("status")
	q.Ref = r.URL.Query().Get("ref")
	q.Locus = r.URL.Query().Get("locus")
	q.Source = r.URL.Query().Get("source")
	q.After = after
	est, err := s.store.EstimateChanges(r.Context(), q)
	if err != nil {
		s.readFailed(w, r, adapter, err)
		return nil, false, false
	}
	budget, admitted := s.admitMemory(w, r, est)
	if !admitted {
		return nil, false, false
	}
	q.MaxRowTextBytes = budget
	changes, truncated, err = s.store.QueryChanges(r.Context(), q)
	if err != nil {
		s.readFailed(w, r, adapter, err)
		return nil, false, false
	}
	return changes, truncated, true
}

func (s *Server) queryChanges(w http.ResponseWriter, r *http.Request) {
	win, err := windowFromRFC3339(r)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	key, paged, err := changeCursor(r)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	var after *postgres.ChangeKey
	if paged {
		after = &key
	}
	trace.SpanFromContext(r.Context()).SetAttributes(
		attribute.String("horopter.adapter", "neutral"))
	changes, truncated, ok := s.changesFor(w, r, win, "neutral", after)
	if !ok {
		return
	}
	markTruncated(w, truncated)
	body := map[string]any{"changes": changesToJSON(changes)}
	if truncated && len(changes) > 0 {
		last := changes[len(changes)-1]
		lastKey := []string{last.Start.UTC().Format(time.RFC3339Nano), last.Ref.String()}
		if err := setNext(w, r, body, lastKey); err != nil {
			s.readFailed(w, r, "neutral", err)
			return
		}
	}
	writeJSON(w, http.StatusOK, body)
}

// changeCursor reads the request's cursor as a change key. ok is false when there is none.
func changeCursor(r *http.Request) (key postgres.ChangeKey, ok bool, err error) {
	token := r.URL.Query().Get("cursor")
	if token == "" {
		return postgres.ChangeKey{}, false, nil
	}
	k, err := decodeCursor(token, r.URL.Query())
	if err != nil {
		return postgres.ChangeKey{}, false, err
	}
	if len(k) != 2 {
		return postgres.ChangeKey{}, false, errBadCursor
	}
	start, err := time.Parse(time.RFC3339Nano, k[0])
	if err != nil {
		return postgres.ChangeKey{}, false, errBadCursor
	}
	return postgres.ChangeKey{Start: start, Ref: k[1]}, true, nil
}

// setNext offers the page after rows, when rows were cut and at least one came back: a page
// with no row has no key to continue from, and repeating the request's own cursor would send
// a client round the same page forever.
func setNext(w http.ResponseWriter, r *http.Request, body map[string]any, key []string) error {
	token, err := encodeCursor(key, r.URL.Query())
	if err != nil {
		return err
	}
	next := nextURL(r, token)
	w.Header().Set("Link", "<"+next+`>; rel="next"`)
	body["next"] = next
	return nil
}

// cursorError is err, or errBadCursor when a token decoded but carried the wrong key shape.
func cursorError(err error) error {
	if err != nil {
		return err
	}
	return errBadCursor
}

// listEvents returns the raw event log for a tenant, optionally scoped to one change's
// ref, for audit and export. It reads only — no write path exists here.
//
// "ref" is a projection ref, matched the way core.RefFor forms one: a correlated event's
// correlation_ref, or an uncorrelated event's locus and own id. See the store's
// ListEvents for why the id arm is restricted to rows carrying no correlation_ref.
func (s *Server) listEvents(w http.ResponseWriter, r *http.Request) {
	limit, err := limitParam(r)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	afterID, err := eventCursor(r)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	tenantID := tenantFromContext(r.Context())
	trace.SpanFromContext(r.Context()).SetAttributes(
		attribute.String("horopter.adapter", "neutral"),
		attribute.String("horopter.tenant", tenantID.String()),
	)
	q, err := postgres.NewEventQuery(tenantID, limit)
	if err != nil {
		s.readFailed(w, r, "neutral", err)
		return
	}
	q.Ref = r.URL.Query().Get("ref")
	q.AfterID = afterID
	est, err := s.store.EstimateEvents(r.Context(), q)
	if err != nil {
		s.readFailed(w, r, "neutral", err)
		return
	}
	budget, admitted := s.admitMemory(w, r, est)
	if !admitted {
		return
	}
	q.MaxRowTextBytes = budget
	events, truncated, err := s.store.ListEvents(r.Context(), q)
	if err != nil {
		s.readFailed(w, r, "neutral", err)
		return
	}
	s.writeEventsPage(w, r, events, truncated)
}

// eventCursor reads the request's cursor as the id the next page starts after; "" when there
// is none.
func eventCursor(r *http.Request) (string, error) {
	token := r.URL.Query().Get("cursor")
	if token == "" {
		return "", nil
	}
	k, err := decodeCursor(token, r.URL.Query())
	if err != nil || len(k) != 1 {
		return "", cursorError(err)
	}
	return k[0], nil
}

// writeEventsPage answers with the events envelope, offering next when rows were cut and at
// least one came back.
func (s *Server) writeEventsPage(
	w http.ResponseWriter, r *http.Request, events []core.Event, truncated bool,
) {
	markTruncated(w, truncated)
	body := map[string]any{"events": eventsToJSON(events)}
	if truncated && len(events) > 0 {
		if err := setNext(w, r, body, []string{events[len(events)-1].ID}); err != nil {
			s.readFailed(w, r, "neutral", err)
			return
		}
	}
	writeJSON(w, http.StatusOK, body)
}

// grafanaAnnotations renders the changes overlapping a window as Grafana annotations.
//
// It refuses "ref" rather than honouring it. The spec adds fetch-by-ref to GET /v1/changes
// alone and leaves this adapter's shape unchanged; the filter takes the full stored
// <locus>/<ref>, which no Grafana dashboard variable can produce; and this route's whole
// contract is "what overlapped this window", which a primary-key lookup ignoring the window
// has no reading for. Accepting it while still demanding from/to — which changeSelection then
// discards — was a second, undocumented spelling of a route that already exists.
func (s *Server) grafanaAnnotations(w http.ResponseWriter, r *http.Request) {
	if r.URL.Query().Get("ref") != "" {
		writeError(w, http.StatusBadRequest,
			`query parameter "ref" is not supported here; this route renders what overlaps `+
				`from/to. Fetch one change by ref with GET /v1/changes?ref=<locus>/<ref>`)
		return
	}
	win, err := windowFromEpochMillis(r)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	s.render(w, r, win, "grafana", s.grafana.Render)
}

func (s *Server) otlpSpans(w http.ResponseWriter, r *http.Request) {
	win, err := windowFromRFC3339(r)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	s.render(w, r, win, "otlp", s.otlp.Render)
}

func (s *Server) render(
	w http.ResponseWriter, r *http.Request, win window, adapter string,
	render func([]core.Change, time.Time) (any, error),
) {
	// Set before the query, not after it: which adapter a request was for is known at
	// entry, and a failing query is exactly when you need to know whether it was Grafana
	// or OTLP asking. Recording it only on success left every failed read unattributed.
	trace.SpanFromContext(r.Context()).SetAttributes(
		attribute.String("horopter.adapter", adapter))

	changes, truncated, ok := s.changesFor(w, r, win, adapter, nil)
	if !ok {
		return
	}
	payload, err := render(changes, s.clock())
	if err != nil {
		s.readFailed(w, r, adapter, err)
		return
	}
	markTruncated(w, truncated)
	writeJSON(w, http.StatusOK, payload)
}

// readFailed answers a read the store, or the adapter rendering its result, could not
// complete, and it draws the same line ingress's appendFailed does. The underlying text
// names tables, columns and constraints — for a failed connection, the database host,
// user and name — and the store wraps it around the caller's own tenant string, so it goes
// to the server log, where an operator can see it, rather than to the caller.
//
// The status stays 500 rather than ingress's 503. There, 503 is what makes a producer
// retry instead of dropping an event; a failed read drops nothing, and its caller — a
// dashboard panel or an export — simply asks again.
func (s *Server) readFailed(w http.ResponseWriter, r *http.Request, adapter string, err error) {
	tenant := requestedTenantFromContext(r.Context())
	slog.ErrorContext(r.Context(), "read failed", "error", err,
		"tenant", tenant, "adapter", adapter, "route", r.Pattern)
	writeError(w, http.StatusInternalServerError,
		"could not complete the read; the cause has been logged on the server")
}

// changesToJSON renders changes in neutral terms. No vendor field names, no epoch
// units — those belong to the adapters.
//
// "start" is the answer to "when did this change start"; first_seen_at and started_at are
// the two facts it is derived from. Both are emitted because the point of recording them
// separately is that first_seen_at < started_at — an event claiming to predate the
// change's declared start — becomes a queryable anomaly, and it cannot be queried if the
// read side never emits it. started_at is omitted while it is NULL, the way "end" is
// omitted while the change is open: absent means "no producer has asserted a start yet",
// which is not the same fact as any timestamp would be.
func changesToJSON(changes []core.Change) []map[string]any {
	out := make([]map[string]any, 0, len(changes))
	for _, c := range changes {
		m := map[string]any{
			"ref": c.Ref.String(), "subject": c.Subject, "type": c.Type, "title": c.Title,
			"actor": c.Actor, "start": c.Start.UTC().Format(time.RFC3339Nano),
			"first_seen_at": c.FirstSeenAt.UTC().Format(time.RFC3339Nano),
			"status":        string(c.Status), "attributes": c.Attributes,
			"last_event_id": c.LastEventID, "sources": c.Sources,
		}
		if c.StartedAt != nil {
			m["started_at"] = c.StartedAt.UTC().Format(time.RFC3339Nano)
		}
		if !c.IsOpen() {
			m["end"] = c.End.UTC().Format(time.RFC3339Nano)
		}
		out = append(out, m)
	}
	return out
}

// eventsToJSON renders the raw event log in neutral terms, for audit and export.
//
// origin and edited are emitted like every other stored column. Both are server-set and
// unreadable from a request body, which is what makes them worth reading back: origin is the
// only record of whether a human or a process appended the event, and audit is this route's
// whole purpose.
//
// lifecycle is emitted for the same reason, more so: it is producer-set, not server-set, and
// it changes how the event folds. Without it a producer auditing its own log cannot tell an
// explicit "lifecycle": "started" from a ".started" type suffix it never sent.
//
// locus is server-set from the appending credential and is what every stored ref is prefixed
// with, so it is the one field that explains why a correlation_ref sent as "dep-1" reads back
// as "ci/dep-1" here. Omitting it left the audit route showing a transformation without its
// input.
func eventsToJSON(events []core.Event) []map[string]any {
	out := make([]map[string]any, 0, len(events))
	for _, e := range events {
		out = append(out, map[string]any{
			"id": e.ID, "tenant": e.Tenant, "idempotency_key": e.IdempotencyKey,
			"schema_version": e.SchemaVersion, "source": e.Source, "type": e.Type,
			"subject": e.Subject, "actor": e.Actor, "title": e.Title,
			"occurred_at":     e.OccurredAt.UTC().Format(time.RFC3339Nano),
			"received_at":     e.ReceivedAt.UTC().Format(time.RFC3339Nano),
			"correlation_ref": e.CorrelationRef.String(), "attributes": e.Attributes,
			"origin": string(e.Origin), "edited": e.Edited,
			"lifecycle": string(e.Lifecycle), "locus": e.Locus.String(),
		})
	}
	return out
}

// markTruncated sets TruncatedHeader when the result set was cut short. It must run
// before writeJSON: WriteHeader flushes the header block, and anything set after it is
// silently dropped.
func markTruncated(w http.ResponseWriter, truncated bool) {
	if truncated {
		w.Header().Set(TruncatedHeader, "true")
	}
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]string{"error": msg})
}
