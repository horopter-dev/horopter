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
		// An open change renders as a region growing to the present; anything else
		// renders to the end it recorded. core.Change.IsOpen is the domain's name for
		// that distinction (spec §5.3), so the adapter asks the change which shape it
		// is rather than re-deriving it from a nil check the concept has a word for.
		// IsOpen() is false exactly when End is non-nil, so the dereference is safe.
		end := now
		if !c.IsOpen() {
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

// textFor returns the annotation body text.
//
// horopter.description is a Horopter extension, not an OTel semantic
// convention — no spec vocabulary names an annotation-body concept. It is
// namespaced under "horopter." the same way otlp.mergedAttributes sets
// horopter.subject, horopter.ref and horopter.status (internal/adapter/otlp),
// which is what keeps a project-specific extension point consistent with
// "attribute keys follow OTel semantic conventions rather than an invented
// vocabulary": the vocabulary itself stays OTel-only, and this is a clearly
// namespaced addition layered on it, not a second invented vocabulary
// competing with it.
//
// Reading a bare, unnamespaced "description" previously captured whatever
// attribute a producer happened to give that name into the annotation body as
// a side effect nobody asked for. A producer that wants annotation body text
// now sets horopter.description explicitly.
func textFor(c core.Change) string {
	if v, ok := c.Attributes["horopter.description"]; ok {
		return fmt.Sprintf("%v", v)
	}
	return ""
}
