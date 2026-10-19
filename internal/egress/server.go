// Package egress is the read-side daemon; it holds a full Store but never calls a write method.
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

// MaxLimit caps every query's result set, by default and on any caller-supplied limit.
const MaxLimit = 1000

// TruncatedHeader marks a response cut short by the limit: vendor body shapes cannot carry a flag.
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

// New returns an egress Server, refusing a nil authenticator: every read route needs one.
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

// Starts at the pool size, may fall to one and rise to 4x: reads spend time rendering, too.
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
	// Empty operation: otelhttp's default span namer ignores it.
	return otelhttp.NewHandler(logging.Requests(mux, logging.WithQuery()), "")
}

// Wraps admission, so an unauthenticated request never spends a read permit.
func (s *Server) authz(c auth.Capability, next http.Handler) http.Handler {
	return s.authn.Require(c)(next)
}

// Must not touch the database: restarting the pod will not fix a database outage.
func (s *Server) healthz(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

// pgx's error names the DB user, name, host and port, so it is logged, not relayed.
func (s *Server) readyz(w http.ResponseWriter, r *http.Request) {
	if err := s.store.Pool().Ping(r.Context()); err != nil {
		slog.ErrorContext(r.Context(), "readiness probe failed", "error", err)
		writeError(w, http.StatusServiceUnavailable, "database unreachable")
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "ready"})
}

var errTenantRequired = errors.New(`query parameter "tenant" is required for this credential`)

var errTenantForbidden = errors.New("credential is scoped to a different tenant")

// Never include the id: a uniform 404 hides which tenants exist.
var errTenantNotFound = errors.New("tenant not found")

// A missing principal fails closed: the zero Principal would read as global.
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

// The ceiling is core.OpenSentinel; beyond it pgx's timestamptz encoding overflows silently.
var (
	minEpochMillis = time.Unix(0, 0).UTC().UnixMilli()
	maxEpochMillis = core.OpenSentinel.UnixMilli()
)

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

type window struct {
	from, to time.Time
	limit    int
}

// The one place from and to are compared: an inverted window selects changes spanning it.
func newWindow(from, to time.Time, limit int) (window, error) {
	if from.After(to) {
		return window{}, fmt.Errorf(
			"query window from=%s is after to=%s; give the range earliest first",
			from.UTC().Format(time.RFC3339), to.UTC().Format(time.RFC3339))
	}
	return window{from: from, to: to, limit: limit}, nil
}

// With a ref, from/to are optional: changeSelection drops the window for a key lookup.
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

// Everything after the prefix is the key, dots included; an empty key is left for the allowlist.
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

// The tenant comes from context: admit resolved it, a DB round trip for a global credential.
func (s *Server) changesFor(
	w http.ResponseWriter, r *http.Request, win window, adapter string, after *postgres.ChangeKey,
) (changes []core.Change, truncated, ok bool) {
	tenantID := tenantFromContext(r.Context())
	trace.SpanFromContext(r.Context()).SetAttributes(
		attribute.String("horopter.tenant", tenantID.String()))
	q, err := postgres.NewChangeQuery(
		tenantID, win.from, win.to, win.limit, attributeFilters(r))
	if err != nil {
		// The caller's mistake: 400 with the store's message, not readFailed's 500.
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

// Only when a cut page returned rows: resending the request's own cursor would loop forever.
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

// ref is a projection ref, as core.RefFor forms one.
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

// Refuses ref: fetch-by-ref belongs to GET /v1/changes, and no dashboard can produce one.
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
	// Set before the query, so a failing read is still attributed to its adapter.
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

// Logs the store error, never relays it; 500 not 503, since a failed read drops nothing.
func (s *Server) readFailed(w http.ResponseWriter, r *http.Request, adapter string, err error) {
	tenant := requestedTenantFromContext(r.Context())
	slog.ErrorContext(r.Context(), "read failed", "error", err,
		"tenant", tenant, "adapter", adapter, "route", r.Pattern)
	writeError(w, http.StatusInternalServerError,
		"could not complete the read; the cause has been logged on the server")
}

// started_at is omitted while NULL: absent and a timestamp are different facts.
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

// Emits every stored column, server-set origin, edited and locus included: this is the audit route.
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

// Must run before writeJSON: WriteHeader flushes the header block.
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
