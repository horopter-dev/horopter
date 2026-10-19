package egress

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"strconv"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/trace"
	"horopter.dev/horopter/internal/admission"
	"horopter.dev/horopter/internal/core"
	"horopter.dev/horopter/internal/store/postgres"
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

// Measured by TestReadAllocationsStayWithinExpansionFactor; set 10% above its largest ratio.
const readExpansionFactor = 46

// Nil in production; tests set it to hold an admitted read in flight.
var afterAdmissionHook func()

// tenant is set only once a permit is held; requestedTenant is unvalidated caller input.
type readState struct {
	requestedTenant       string
	tenant                core.TenantID
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

type readStateKey struct{}

func tenantFromContext(ctx context.Context) core.TenantID {
	state, _ := ctx.Value(readStateKey{}).(*readState)
	if state == nil {
		return core.TenantID{}
	}
	return state.tenant
}

// Raw and unvalidated, but available even when resolution itself failed.
func requestedTenantFromContext(ctx context.Context) string {
	state, _ := ctx.Value(readStateKey{}).(*readState)
	if state == nil {
		return ""
	}
	return state.requestedTenant
}

// Resolves the tenant after the permit: a global credential's lookup hits the database.
func (s *Server) admit(read http.HandlerFunc) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		state := &readState{
			outcome: outcomeAdmitted, requestedTenant: r.URL.Query().Get("tenant"),
		}
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

		tenantID, err := s.tenantFor(r)
		if err != nil {
			writeTenantError(w, r, err)
			return
		}
		state.tenant = tenantID

		if afterAdmissionHook != nil {
			afterAdmissionHook()
		}
		ctx := postgres.WithConnectionWaitObserver(r.Context(),
			state.connectionWaitObserver(r.Context()))
		ctx = context.WithValue(ctx, readStateKey{}, state)
		read(w, r.WithContext(ctx))
	})
}

// A failed acquisition counts only while the request context is live; its wait always counts.
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

func (st *readState) overloaded(threshold time.Duration) bool {
	return st.longestConnectionWait > threshold || st.acquisitionFailed
}

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

// Every shed read is logged: shedding happens far below the rate a log line can be written.
func (s *Server) shed(
	w http.ResponseWriter, r *http.Request, state *readState, gate string, figures ...any,
) {
	retryAfter := retryAfterSeconds(s.limits.AdmissionWait)
	fields := []any{"route", r.Pattern, "tenant", state.requestedTenant, "gate", gate,
		"waited_ms", state.waited.Milliseconds(), "retry_after_s", retryAfter}
	slog.WarnContext(r.Context(), "read shed", append(fields, figures...)...)
	s.metrics.shed.Add(r.Context(), 1,
		metric.WithAttributes(attribute.String("horopter.read.shed.gate", gate)))
	w.Header().Set("Retry-After", strconv.Itoa(retryAfter))
	writeError(w, http.StatusServiceUnavailable,
		"egress is at capacity; retry after the interval in Retry-After")
}

func retryAfterSeconds(wait time.Duration) int {
	return max(1, int((wait+time.Second-1)/time.Second))
}

// A read shed at the concurrency gate acquired no connection, so it carries no wait.
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

// A first row larger than the pool is refused 422; a larger read is cut to what fits.
func (s *Server) admitMemory(
	w http.ResponseWriter, r *http.Request, est postgres.ReadEstimate,
) (budget int64, admitted bool) {
	state, _ := r.Context().Value(readStateKey{}).(*readState)
	state.estimated, state.estimatedBytes = true, est.TotalBytes
	capacity := s.memory.Capacity()
	if est.FirstRowBytes*readExpansionFactor > capacity {
		state.outcome = outcomeTooLarge
		s.tooLarge(w, r, state, est)
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

// The body names the limit, which is configuration, and nothing from the store.
func (s *Server) tooLarge(
	w http.ResponseWriter, r *http.Request, state *readState, est postgres.ReadEstimate,
) {
	capacity := s.memory.Capacity()
	slog.WarnContext(r.Context(), "read too large", "route", r.Pattern,
		"tenant", state.requestedTenant, "first_row_bytes", est.FirstRowBytes,
		"needed_bytes", est.FirstRowBytes*readExpansionFactor, "capacity_bytes", capacity)
	writeError(w, http.StatusUnprocessableEntity, fmt.Sprintf(
		"the first row of this read is larger than egress's read memory limit of %d bytes "+
			"allows; narrow the read to a subject, type or status that row does not have "+
			"(on /v1/events, to a ref), or raise HOROPTER_EGRESS_READ_MEMORY_BYTES",
		capacity))
}
