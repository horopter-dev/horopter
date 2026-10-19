// Package otlp renders changes as OTLP/JSON spans, in nanoseconds with typed attributes.
package otlp

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"sort"
	"strconv"
	"time"

	"horopter.dev/horopter/internal/adapter"
	"horopter.dev/horopter/internal/core"
)

// ExportRequest is the OTLP/JSON ExportTraceServiceRequest that Render returns.
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

// ScopeSpans is a collection of Spans; the instrumentation scope is left unset.
type ScopeSpans struct {
	Spans []Span `json:"spans"`
}

// Span is the OTLP/JSON shape of one span. It has no kind: INTERNAL would claim a call graph.
type Span struct {
	TraceID           string     `json:"traceId"`
	SpanID            string     `json:"spanId"`
	Name              string     `json:"name"`
	StartTimeUnixNano string     `json:"startTimeUnixNano"`
	EndTimeUnixNano   string     `json:"endTimeUnixNano"`
	Attributes        []KeyValue `json:"attributes"`
	Status            *Status    `json:"status,omitempty"`
}

// Status is OTLP's span status; a nil Status on Span is OTLP's STATUS_CODE_UNSET.
type Status struct {
	Code int `json:"code"`
}

// OTLP/JSON requires an enum as its integer value, unlike the protobuf JSON mapping.
const (
	statusCodeOK    = 1
	statusCodeError = 2
)

// KeyValue is one OTLP attribute: a key and a typed AnyValue.
type KeyValue struct {
	Key   string   `json:"key"`
	Value AnyValue `json:"value"`
}

// AnyValue is OTLP's typed attribute-value union: one field set, or none for no value.
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

// KeyValueList is a list of KeyValue, for an attribute whose Go value is a map[string]any.
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
		// IsOpen() is false exactly when End is non-nil.
		end := now
		if !c.IsOpen() {
			end = *c.End
		}
		spans = append(spans, Span{
			TraceID:           traceID(c.Tenant, c.Ref.String()),
			SpanID:            spanID(c.Tenant, c.Ref.String()),
			Name:              c.Title,
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

// resource names Horopter, the exporter, as service.name; a change's subject is an attribute.
func resource() Resource {
	name := "horopter"
	return Resource{Attributes: []KeyValue{{Key: "service.name", Value: AnyValue{StringValue: &name}}}}
}

// mergedAttributes overrides a producer's attributes of the same name as a horopter.* key.
func mergedAttributes(c core.Change) map[string]any {
	attrs := make(map[string]any, len(c.Attributes)+3)
	for k, v := range c.Attributes {
		attrs[k] = v
	}
	attrs["horopter.subject"] = c.Subject
	attrs["horopter.ref"] = c.Ref.String()
	attrs["horopter.status"] = string(c.Status)
	return attrs
}

// attributesFor converts a map into a KeyValue list sorted by key, for deterministic output.
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

// anyValueFor is at the cyclomatic-complexity ceiling of 8: split it before adding a case.
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

// formatUnixNano writes a decimal string: OTLP/JSON encodes every 64-bit integer as one.
func formatUnixNano(t time.Time) string {
	return strconv.FormatInt(t.UnixNano(), 10)
}

// otelStatus leaves a cancelled change unset, not Error: it was stopped, not failed.
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

// traceID is tagged apart from spanID, else the span id would be a prefix of the trace id.
func traceID(tenant, ref string) string {
	return derive("trace:"+strconv.Itoa(len(tenant))+":"+tenant+"/"+ref, 16)
}

func spanID(tenant, ref string) string {
	return derive("span:"+strconv.Itoa(len(tenant))+":"+tenant+"/"+ref, 8)
}

// derive produces a stable hex id of n bytes, so re-exporting a change repeats its span.
func derive(key string, n int) string {
	sum := sha256.Sum256([]byte(key))
	return hex.EncodeToString(sum[:n])
}
