// Package otlp renders changes as OTLP/JSON spans. One adapter reaches every
// OTel-compatible backend, which is better leverage than a per-vendor adapter.
//
// OTLP uses nanoseconds and keeps attributes typed. Grafana uses milliseconds
// and flattens them to strings. That both are correct here, and differ, is the
// evidence that the adapter boundary is real.
package otlp

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"sort"
	"strconv"
	"time"

	"github.com/yo61/horopter/internal/adapter"
	"github.com/yo61/horopter/internal/core"
)

// ExportRequest is the JSON shape of an OTLP ExportTraceServiceRequest — the
// envelope a collector's /v1/traces JSON endpoint expects, and what Render
// returns as this package's payload. See opentelemetry-proto's
// collector/trace/v1/trace_service.proto and trace/v1/trace.proto for the
// message definitions, and the OTLP spec's JSON Protobuf Encoding section for
// the wire mapping: https://opentelemetry.io/docs/specs/otlp/#json-protobuf-encoding
type ExportRequest struct {
	ResourceSpans []ResourceSpans `json:"resourceSpans"`
}

// ResourceSpans is a collection of ScopeSpans from one Resource.
type ResourceSpans struct {
	Resource   Resource     `json:"resource"`
	ScopeSpans []ScopeSpans `json:"scopeSpans"`
}

// Resource describes the entity emitting the spans.
type Resource struct {
	Attributes []KeyValue `json:"attributes"`
}

// ScopeSpans is a collection of Spans from one instrumentation scope. This
// adapter has no instrumentation-scope concept of its own, so scope is left
// unset — OTLP treats an absent scope as an unknown/unnamed one, which is an
// honest description of what Horopter has to say about it.
type ScopeSpans struct {
	Spans []Span `json:"spans"`
}

// Span is the OTLP/JSON shape of one span.
//
// StartTimeUnixNano and EndTimeUnixNano are decimal strings, not JSON numbers:
// the protobuf JSON mapping encodes every 64-bit integer field this way,
// because a nanosecond epoch timestamp exceeds JavaScript's safe integer
// range. traceId and spanId are hex, case-insensitive, per the same spec.
// https://opentelemetry.io/docs/specs/otlp/#json-protobuf-encoding
//
// There is no Kind field: none of OTel's span kinds (SERVER, CLIENT, PRODUCER,
// CONSUMER) describe a boundary an infrastructure change crosses, and INTERNAL
// asserts a call-graph relationship this data doesn't have either. trace.proto
// says of the zero value, SPAN_KIND_UNSPECIFIED: "Do NOT use as default" —
// omitting the field disregards that instruction, it does not comply with it,
// and implementations are only ever told they MAY (not must) treat an
// unspecified kind as INTERNAL on receipt. This is a deliberate trade-off
// between two imperfect options, not spec compliance: an omitted field a
// receiver MAY reinterpret however it likes, versus a populated
// SPAN_KIND_INTERNAL that flatly overclaims a call-graph relationship. Emitting
// "kind": 1 is one line away, in Render, if that trade-off is judged the wrong
// way round.
type Span struct {
	TraceID           string     `json:"traceId"`
	SpanID            string     `json:"spanId"`
	Name              string     `json:"name"`
	StartTimeUnixNano string     `json:"startTimeUnixNano"`
	EndTimeUnixNano   string     `json:"endTimeUnixNano"`
	Attributes        []KeyValue `json:"attributes"`
	Status            *Status    `json:"status,omitempty"`
}

// Status is OTLP's span status. Status is a pointer on Span and omitted
// entirely for the "no definitive outcome recorded" case, matching the OTLP
// spec's own statement that an absent Status means STATUS_CODE_UNSET.
type Status struct {
	Code int `json:"code"`
}

// The Status.Code values, from trace.proto's StatusCode enum. OTLP/JSON
// deviates from the standard protobuf JSON mapping by requiring every enum as
// its integer value; the name strings ("STATUS_CODE_OK") MUST NOT be used.
// https://opentelemetry.io/docs/specs/otlp/#json-protobuf-encoding
const (
	statusCodeOK    = 1
	statusCodeError = 2
)

// KeyValue is one OTLP attribute: a key and a typed AnyValue. Attributes are a
// list of these, never a JSON object — AnyValue is itself a typed union, which
// a plain JSON object cannot represent.
type KeyValue struct {
	Key   string   `json:"key"`
	Value AnyValue `json:"value"`
}

// AnyValue is OTLP's typed attribute-value union: at most one field is set,
// mirroring the protobuf oneof it encodes — zero fields set is the documented
// nil case below, not a bug. IntValue is a decimal string for the same 64-bit
// reason as the span's timestamp fields; real traffic never reaches it,
// though, because attributes arrive from Postgres jsonb via encoding/json,
// which decodes every JSON number as float64, so a numeric attribute is
// always emitted as doubleValue in production. The int case below exists for
// a non-database caller (or a test) that hands this package a Go int
// directly — worth keeping, but not evidence that intValue is on the live
// path. A Go value with no clean member of this union — anything that is not
// a string, bool, int, float64, []any or map[string]any — falls back to its
// fmt "%v" string form in StringValue: lossy, but predictable and never a
// render failure. A Go nil maps to a wholly empty AnyValue, which OTel
// defines as the union's own "no value" case, rather than to the misleading
// string "<nil>".
type AnyValue struct {
	StringValue *string       `json:"stringValue,omitempty"`
	BoolValue   *bool         `json:"boolValue,omitempty"`
	IntValue    *string       `json:"intValue,omitempty"`
	DoubleValue *float64      `json:"doubleValue,omitempty"`
	ArrayValue  *ArrayValue   `json:"arrayValue,omitempty"`
	KvlistValue *KeyValueList `json:"kvlistValue,omitempty"`
}

// ArrayValue is a list of AnyValue, for an attribute whose Go value is []any.
type ArrayValue struct {
	Values []AnyValue `json:"values"`
}

// KeyValueList is a list of KeyValue, for an attribute whose Go value is a
// nested map[string]any.
type KeyValueList struct {
	Values []KeyValue `json:"values"`
}

// Renderer renders changes as OTLP spans.
type Renderer struct{}

var _ adapter.Renderer = (*Renderer)(nil)

// New returns an OTLP renderer.
func New() *Renderer { return &Renderer{} }

// Render converts changes into an OTLP ExportRequest.
func (r *Renderer) Render(changes []core.Change, now time.Time) (any, error) {
	spans := make([]Span, 0, len(changes))
	for _, c := range changes {
		// An open change spans to the present; anything else ends where it recorded.
		// A change takes one of three shapes — point, open, or closed — and
		// core.Change.IsOpen is the domain's name for the open/not-open half of that, so
		// the adapter asks the change which shape it is rather than re-deriving it from
		// a nil check — the same call the Grafana adapter makes for the same reason.
		// IsOpen() is false exactly when End is non-nil, so the dereference is safe.
		end := now
		if !c.IsOpen() {
			end = *c.End
		}
		name := c.Title
		if name == "" {
			name = c.Type
		}
		spans = append(spans, Span{
			TraceID:           traceID(c.Tenant, c.Ref),
			SpanID:            spanID(c.Tenant, c.Ref),
			Name:              name,
			StartTimeUnixNano: formatUnixNano(c.Start),
			EndTimeUnixNano:   formatUnixNano(end),
			Attributes:        attributesFor(mergedAttributes(c)),
			Status:            otelStatus(c.Status),
		})
	}
	return ExportRequest{
		ResourceSpans: []ResourceSpans{{
			Resource:   resource(),
			ScopeSpans: []ScopeSpans{{Spans: spans}},
		}},
	}, nil
}

// resource identifies Horopter itself as the emitter of these spans, not any
// one change's subject. Real OTel backends group and index traces by the
// resource's service.name, and an unset resource files spans under an
// "unknown service" bucket in most of them; Horopter is what is exporting
// this data, exactly as a webhook-to-OTLP bridge's own service.name would be,
// while each span's own subject already travels in its horopter.subject
// attribute.
func resource() Resource {
	name := "horopter"
	return Resource{Attributes: []KeyValue{{Key: "service.name", Value: AnyValue{StringValue: &name}}}}
}

// mergedAttributes overlays a change's own attributes with three horopter.*
// extension keys, so a producer's own attribute of the same name is
// overridden rather than silently duplicated under one key.
func mergedAttributes(c core.Change) map[string]any {
	attrs := make(map[string]any, len(c.Attributes)+3)
	for k, v := range c.Attributes {
		attrs[k] = v
	}
	attrs["horopter.subject"] = c.Subject
	attrs["horopter.ref"] = c.Ref
	attrs["horopter.status"] = string(c.Status)
	return attrs
}

// attributesFor converts a map into a deterministically ordered KeyValue list:
// sorted by key, since Go map iteration order is not deterministic and
// grafana.tagsFor sorts for exactly the same reason.
func attributesFor(m map[string]any) []KeyValue {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	out := make([]KeyValue, 0, len(keys))
	for _, k := range keys {
		out = append(out, KeyValue{Key: k, Value: anyValueFor(m[k])})
	}
	return out
}

// anyValueFor maps a Go attribute value onto OTLP's AnyValue union. See
// AnyValue's doc comment for the fallback and nil rules.
//
// This switch sits at exactly 8, this project's cyclomatic-complexity ceiling
// (confirmed with gocyclo). Adding another AnyValue member — another case —
// pushes it over, so restructure first (e.g. split the numeric or the
// recursive cases into a helper) rather than bolting one more branch on.
func anyValueFor(v any) AnyValue {
	switch t := v.(type) {
	case nil:
		return AnyValue{}
	case string:
		return AnyValue{StringValue: &t}
	case bool:
		return AnyValue{BoolValue: &t}
	case int:
		s := strconv.FormatInt(int64(t), 10)
		return AnyValue{IntValue: &s}
	case float64:
		return AnyValue{DoubleValue: &t}
	case map[string]any:
		return AnyValue{KvlistValue: &KeyValueList{Values: attributesFor(t)}}
	case []any:
		return AnyValue{ArrayValue: &ArrayValue{Values: arrayValuesFor(t)}}
	default:
		s := fmt.Sprintf("%v", t)
		return AnyValue{StringValue: &s}
	}
}

func arrayValuesFor(items []any) []AnyValue {
	vals := make([]AnyValue, 0, len(items))
	for _, item := range items {
		vals = append(vals, anyValueFor(item))
	}
	return vals
}

// formatUnixNano renders a nanosecond epoch timestamp as a decimal string —
// see ExportRequest's doc comment for why.
func formatUnixNano(t time.Time) string {
	return strconv.FormatInt(t.UnixNano(), 10)
}

// otelStatus maps a change's status to OTel's tri-state span status. In
// progress and cancelled deliberately map to nil (Status omitted, OTLP's
// unset) rather than Error: OTel's Error means the operation failed, and a
// cancelled change was deliberately stopped, not failed, so mapping it to
// Error would misreport a deliberate stop as a failure in every downstream
// OTel backend. Omitting Status — "no definitive outcome recorded" — fits
// both a still-open change and a cancelled one.
func otelStatus(s core.Status) *Status {
	switch s {
	case core.StatusCompleted:
		return &Status{Code: statusCodeOK}
	case core.StatusFailed, core.StatusRolledBack:
		return &Status{Code: statusCodeError}
	default:
		return nil
	}
}

// traceID and spanID both fold in the tenant, not just the ref: two tenants
// that happen to share a ref must not produce the same id. They use distinct
// domain-tag prefixes ("trace:"/"span:") so that neither id is a byte-prefix
// of the other — hashing the same joined string at two truncation lengths
// would otherwise make the 8-byte span id a prefix of the 16-byte trace id,
// an accidental correlation nobody asked for.
//
// tenant is length-prefixed the same way idempotencyKey
// (internal/ingress/cloudevents.go) length-prefixes source, so the join stays
// injective: tenant "a/b" with ref "c" cannot collide with tenant "a" and ref
// "b/c" under a plain "/" join.
func traceID(tenant, ref string) string {
	return derive("trace:"+strconv.Itoa(len(tenant))+":"+tenant+"/"+ref, 16)
}

func spanID(tenant, ref string) string {
	return derive("span:"+strconv.Itoa(len(tenant))+":"+tenant+"/"+ref, 8)
}

// derive produces a stable hex id of n bytes from a key, so that re-exporting
// the same change yields the same span rather than a duplicate.
func derive(key string, n int) string {
	sum := sha256.Sum256([]byte(key))
	return hex.EncodeToString(sum[:n])
}
