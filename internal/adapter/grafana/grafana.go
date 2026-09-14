// Package grafana renders changes as Grafana JSON-datasource annotations.
//
// Grafana wants epoch milliseconds and a flat []string of tags. Datadog wants
// epoch seconds. Neither is neutral, which is why the conversion lives here.
package grafana

import (
	"fmt"
	"sort"
	"time"

	"github.com/yo61/horopter/internal/adapter"
	"github.com/yo61/horopter/internal/core"
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
		end := now
		if c.End != nil {
			end = *c.End
		}
		title := c.Title
		if title == "" {
			title = c.Type
		}
		out = append(out, Annotation{
			Time:    c.Start.UnixMilli(),
			TimeEnd: end.UnixMilli(),
			Title:   title,
			Tags:    tagsFor(c),
			Text:    textFor(c),
		})
	}
	return out, nil
}

// tagsFor flattens structured attributes into Grafana's flat tag list. Sorted so
// that output is deterministic: Go map iteration order is not.
func tagsFor(c core.Change) []string {
	tags := make([]string, 0, len(c.Attributes)+2)
	for k, v := range c.Attributes {
		tags = append(tags, fmt.Sprintf("%s:%v", k, v))
	}
	if c.Status != "" {
		tags = append(tags, "status:"+string(c.Status))
	}
	if c.Subject != "" {
		tags = append(tags, "subject:"+c.Subject)
	}
	sort.Strings(tags)
	return tags
}

func textFor(c core.Change) string {
	if v, ok := c.Attributes["description"]; ok {
		return fmt.Sprintf("%v", v)
	}
	return ""
}
