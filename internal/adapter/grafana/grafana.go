// Package grafana renders changes as Grafana JSON-datasource annotations, in epoch milliseconds.
package grafana

import (
	"fmt"
	"sort"
	"time"

	"horopter.dev/horopter/internal/adapter"
	"horopter.dev/horopter/internal/core"
)

// Annotation is Grafana's JSON-datasource annotation shape.
type Annotation struct {
	Time    int64    `json:"time"`
	TimeEnd int64    `json:"timeEnd"`
	Title   string   `json:"title"`
	Tags    []string `json:"tags"`
	Text    string   `json:"text"`
}

// Renderer renders changes as Grafana annotations.
type Renderer struct{}

var _ adapter.Renderer = (*Renderer)(nil)

// New returns a Grafana renderer.
func New() *Renderer { return &Renderer{} }

// Render converts changes into Grafana annotations.
func (r *Renderer) Render(changes []core.Change, now time.Time) (any, error) {
	out := make([]Annotation, 0, len(changes))
	for _, c := range changes {
		// IsOpen() is false exactly when End is non-nil.
		end := now
		if !c.IsOpen() {
			end = *c.End
		}
		out = append(out, Annotation{
			Time:    c.Start.UnixMilli(),
			TimeEnd: end.UnixMilli(),
			Title:   c.Title,
			Tags:    tagsFor(c),
			Text:    textFor(c),
		})
	}
	return out, nil
}

// tagsFor overlays status and subject: an attribute named "status" once rendered a second tag.
func tagsFor(c core.Change) []string {
	merged := make(map[string]any, len(c.Attributes)+2)
	for k, v := range c.Attributes {
		merged[k] = v
	}
	if c.Status != "" {
		merged["status"] = string(c.Status)
	}
	if c.Subject != "" {
		merged["subject"] = c.Subject
	}
	tags := make([]string, 0, len(merged))
	for k, v := range merged {
		tags = append(tags, fmt.Sprintf("%s:%v", k, v))
	}
	sort.Strings(tags)
	return tags
}

// textFor reads horopter.description only: a bare "description" once leaked into the body.
func textFor(c core.Change) string {
	if v, ok := c.Attributes["horopter.description"]; ok {
		return fmt.Sprintf("%v", v)
	}
	return ""
}
