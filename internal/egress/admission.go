package egress

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"strconv"
	"time"

	"github.com/yo61/horopter/internal/admission"
	"github.com/yo61/horopter/internal/store/postgres"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"
)

// The outcomes a gated read ends in, recorded on its server span as horopter.admission.outcome.
const (
	outcomeAdmitted        = "admitted"
	outcomeShedConcurrency = "shed_concurrency"
	outcomeShedMemory      = "shed_memory"
	outcomeTooLarge        = "too_large"
)

// The gates a read can be shed at, as the "gate" log field names them.
const (
	gateConcurrency = "concurrency"
	gateMemory      = "memory"
)

// readExpansionFactor is how many bytes of heap a read may allocate per byte of row text it was
// estimated at. TestReadAllocationsStayWithinExpansionFactor measures the ratio on the costliest
// data shapes through every rendering, and fails if any ratio exceeds this. It is set to the
// smallest whole number at least 10% above the largest ratio that test measured. Allocation, not
// peak heap, is measured: a read cannot add more to the live heap than it allocates, and
// allocation is repeatable where peak heap depends on when the collector ran.
const readExpansionFactor = 46

// afterAdmissionHook is nil in production. Tests set it to hold an admitted read in flight so
// the next read finds the concurrency limit full.
var afterAdmissionHook func()

// readState is what admission learned about one read, recorded on the read's server span when
// the read finishes. The connection-wait observer fills in longestConnectionWait and
// acquisitionFailed; it runs on the read's own goroutine, as pgxpool acquires connections on the
// caller's. admitMemory fills in estimated, estimatedBytes, reserved, reservedBytes and byteCut.
type readState struct {
	outcome               string
	waited                time.Duration
	longestConnectionWait time.Duration
	acquisitionFailed     bool
	estimated             bool
	estimatedBytes        int64
	reserved              bool
	reservedBytes         int64
	byteCut               bool
}

// readStateKey is the context key admit attaches a read's *readState under, so admitMemory can
// reach it without threading it through every handler signature.
type readStateKey struct{}

// admit gates a read route on the concurrency limit. A read that gets no permit within
// Limits.AdmissionWait is shed with 503; an admitted read holds its permit until its response
// has been written. It also attaches the read's state and connection-wait observer to the
// context the handler runs with, which the handler's own memory gate (admitMemory) uses.
func (s *Server) admit(read http.HandlerFunc) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		state := &readState{outcome: outcomeAdmitted}
		defer state.record(r.Context())

		started := time.Now()
		waitCtx, cancel := context.WithTimeout(r.Context(), s.limits.AdmissionWait)
		permit, err := s.limiter.Acquire(waitCtx)
		cancel()
		state.waited = time.Since(started)
		if err != nil {
			state.outcome = outcomeShedConcurrency
			s.shed(w, r, state, gateConcurrency,
				"limit", s.limiter.Limit(), "in_flight", s.limiter.InFlight())
			return
		}
		defer s.release(r.Context(), permit, state)

		if afterAdmissionHook != nil {
			afterAdmissionHook()
		}
		ctx := postgres.WithConnectionWaitObserver(r.Context(),
			state.connectionWaitObserver(r.Context()))
		ctx = context.WithValue(ctx, readStateKey{}, state)
		read(w, r.WithContext(ctx))
	})
}

// connectionWaitObserver returns the observer admit attaches to an admitted read's context. A
// failed acquisition counts against the database only while the request's own context is live:
// a client that disconnects ends that context, which says nothing about the database. However
// the acquisition ended, its wait still counts.
func (st *readState) connectionWaitObserver(
	requestCtx context.Context,
) postgres.ConnectionWaitObserver {
	return func(wait time.Duration, err error) {
		st.longestConnectionWait = max(st.longestConnectionWait, wait)
		if err != nil && requestCtx.Err() == nil {
			st.acquisitionFailed = true
		}
	}
}

// overloaded reports whether the read found the database pool congested: its longest wait for a
// connection exceeded threshold, or an acquisition failed.
func (st *readState) overloaded(threshold time.Duration) bool {
	return st.longestConnectionWait > threshold || st.acquisitionFailed
}

// release ends an admitted read: it gives back the read's memory reservation, if any, then
// returns its concurrency permit, reporting whether the read found the pool congested, and logs
// the limit if that halved it.
func (s *Server) release(ctx context.Context, permit admission.Permit, state *readState) {
	if state.reserved {
		s.memory.Release(state.reservedBytes)
	}
	before, after := permit.Release(state.overloaded(s.limits.DBWaitThreshold))
	if after < before {
		slog.WarnContext(ctx, limitHalvedMessage(before, after, state),
			"from", before, "to", after,
			"connection_wait_ms", state.longestConnectionWait.Milliseconds(),
			"acquisition_failed", state.acquisitionFailed)
	}
}

// limitHalvedMessage says, in a line that reads on its own, what halved the limit.
func limitHalvedMessage(before, after int, state *readState) string {
	if state.acquisitionFailed {
		return fmt.Sprintf("read concurrency limit halved from %d to %d after a failed "+
			"database connection acquisition", before, after)
	}
	return fmt.Sprintf("read concurrency limit halved from %d to %d after a %dms database "+
		"connection wait", before, after, state.longestConnectionWait.Milliseconds())
}

// shed answers a read refused at gate with 503 and Retry-After, and logs it at WARN with the
// gate's figures. Every shed read is logged: shedding is driven by database capacity, far below
// the rate at which the handler can write a log line, and a volumetric flood is expected to be
// stopped in front of Horopter.
func (s *Server) shed(
	w http.ResponseWriter, r *http.Request, state *readState, gate string, figures ...any,
) {
	tenantID, _ := tenantOf(r)
	retryAfter := retryAfterSeconds(s.limits.AdmissionWait)
	fields := []any{"route", r.Pattern, "tenant", tenantID, "gate", gate,
		"waited_ms", state.waited.Milliseconds(), "retry_after_s", retryAfter}
	slog.WarnContext(r.Context(), "read shed", append(fields, figures...)...)
	w.Header().Set("Retry-After", strconv.Itoa(retryAfter))
	writeError(w, http.StatusServiceUnavailable,
		"egress is at capacity; retry after the interval in Retry-After")
}

// retryAfterSeconds is the Retry-After a shed read is given: the admission wait rounded up to
// whole seconds, and at least one.
func retryAfterSeconds(wait time.Duration) int {
	return max(1, int((wait+time.Second-1)/time.Second))
}

// record sets what admission decided on the read's server span. A read shed at the concurrency
// gate acquired no connection, so it carries no connection wait.
func (st *readState) record(ctx context.Context) {
	attrs := []attribute.KeyValue{
		attribute.String("horopter.admission.outcome", st.outcome),
		attribute.Int64("horopter.admission.wait_ms", st.waited.Milliseconds()),
	}
	if st.outcome != outcomeShedConcurrency {
		attrs = append(attrs, attribute.Int64("horopter.db.connection_wait_ms",
			st.longestConnectionWait.Milliseconds()))
	}
	if st.estimated {
		attrs = append(attrs, attribute.Int64("horopter.read.estimated_bytes", st.estimatedBytes))
	}
	if st.reserved {
		attrs = append(attrs,
			attribute.Int64("horopter.read.reserved_bytes", st.reservedBytes),
			attribute.Bool("horopter.read.byte_cut", st.byteCut))
	}
	trace.SpanFromContext(ctx).SetAttributes(attrs...)
}

// admitMemory decides whether a read estimated at est can be served, and reserves its memory
// until the read is released. It returns the row-text budget to cut the read's query at, zero
// for none, and false when it has already answered the request.
//
// A read whose first row alone needs more than the pool can never be served, and is refused with
// 422. A read needing more than the whole pool reserves all of it and is cut to what the pool
// holds, so it returns the oldest rows that fit, marked truncated, rather than failing.
func (s *Server) admitMemory(
	w http.ResponseWriter, r *http.Request, est postgres.ReadEstimate,
) (budget int64, admitted bool) {
	state, _ := r.Context().Value(readStateKey{}).(*readState)
	state.estimated, state.estimatedBytes = true, est.TotalBytes
	capacity := s.memory.Capacity()
	if est.FirstRowBytes*readExpansionFactor > capacity {
		state.outcome = outcomeTooLarge
		s.tooLarge(w, r, est)
		return 0, false
	}
	needed := est.TotalBytes * readExpansionFactor
	cut := needed > capacity
	if cut {
		needed = capacity
	}

	started := time.Now()
	waitCtx, cancel := context.WithTimeout(r.Context(), s.limits.AdmissionWait)
	err := s.memory.Reserve(waitCtx, needed)
	cancel()
	state.waited += time.Since(started)
	if err != nil {
		state.outcome = outcomeShedMemory
		s.shed(w, r, state, gateMemory, "needed_bytes", needed,
			"reserved_bytes", s.memory.Reserved(), "capacity_bytes", capacity)
		return 0, false
	}
	state.reserved, state.reservedBytes, state.byteCut = true, needed, cut
	if cut {
		return capacity / readExpansionFactor, true
	}
	return 0, true
}

// tooLarge answers a read whose first row alone needs more memory than the pool holds. The body
// names the limit, which is configuration, and nothing from the store.
func (s *Server) tooLarge(w http.ResponseWriter, r *http.Request, est postgres.ReadEstimate) {
	tenantID, _ := tenantOf(r)
	capacity := s.memory.Capacity()
	slog.WarnContext(r.Context(), "read too large", "route", r.Pattern, "tenant", tenantID,
		"first_row_bytes", est.FirstRowBytes,
		"needed_bytes", est.FirstRowBytes*readExpansionFactor, "capacity_bytes", capacity)
	writeError(w, http.StatusUnprocessableEntity, fmt.Sprintf(
		"the first row of this read is larger than egress's read memory limit of %d bytes "+
			"allows; filter it out of the query, or raise HOROPTER_EGRESS_READ_MEMORY_BYTES",
		capacity))
}
