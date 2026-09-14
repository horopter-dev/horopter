// Package otlp renders changes as OTLP-shaped spans. One adapter reaches every
// OTel-compatible backend, which is better leverage than a per-vendor adapter.
//
// OTLP uses nanoseconds and keeps attributes typed. Grafana uses milliseconds
// and flattens them to strings. That both are correct here, and differ, is the
// evidence that the adapter boundary is real.
package otlp

import (
	"crypto/sha256"
	"encoding/hex"
	"time"

	"github.com/yo61/horopter/internal/adapter"
	"github.com/yo61/horopter/internal/core"
)

// Span is the subset of an OTLP span that a change maps onto.
type Span struct {
	TraceID       string
	SpanID        string
	Name          string
	StartUnixNano int64
	EndUnixNano   int64
	Attributes    map[string]any
	Status        string
}

// Renderer renders changes as OTLP spans.
type Renderer struct{}

var _ adapter.Renderer = (*Renderer)(nil)

// New returns an OTLP renderer.
func New() *Renderer { return &Renderer{} }

// Render converts changes into OTLP spans.
func (r *Renderer) Render(changes []core.Change, now time.Time) (any, error) {
	out := make([]Span, 0, len(changes))
	for _, c := range changes {
		end := now
		if c.End != nil {
			end = *c.End
		}
		attrs := make(map[string]any, len(c.Attributes)+3)
		for k, v := range c.Attributes {
			attrs[k] = v
		}
		attrs["horopter.subject"] = c.Subject
		attrs["horopter.ref"] = c.Ref
		attrs["horopter.status"] = string(c.Status)

		name := c.Title
		if name == "" {
			name = c.Type
		}
		out = append(out, Span{
			TraceID:       derive(c.Tenant+"/"+c.Ref, 16),
			SpanID:        derive(c.Ref, 8),
			Name:          name,
			StartUnixNano: c.Start.UnixNano(),
			EndUnixNano:   end.UnixNano(),
			Attributes:    attrs,
			Status:        otelStatus(c.Status),
		})
	}
	return out, nil
}

func otelStatus(s core.Status) string {
	switch s {
	case core.StatusCompleted:
		return "Ok"
	case core.StatusFailed, core.StatusRolledBack:
		return "Error"
	default:
		return "Unset"
	}
}

// derive produces a stable hex id of n bytes from a key, so that re-exporting
// the same change yields the same span rather than a duplicate.
func derive(key string, n int) string {
	sum := sha256.Sum256([]byte(key))
	return hex.EncodeToString(sum[:n])
}
