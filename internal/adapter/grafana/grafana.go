// Package grafana renders changes as Grafana JSON-datasource annotations.
//
// Grafana wants epoch milliseconds and a flat []string of tags. Datadog wants
// epoch seconds. Neither is neutral, which is why the conversion lives here.
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
		// An open change renders as a region growing to the present; anything else
		// renders to the end it recorded. A change takes one of three shapes — point,
		// open, or closed — and core.Change.IsOpen is the domain's name for the
		// distinction, so the adapter asks the change which shape it is rather than
		// re-deriving it from a nil check the concept has a word for.
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
// The computed tags OVERLAY a producer's attributes of the same name rather than being
// appended beside them. Grafana tags are a flat list, so an attribute literally named
// "status" used to render a second "status:" tag next to the change's own, with nothing
// marking which one is authoritative. Nothing restricts attribute keys, so that is
// reachable from either ingest path. otlp.mergedAttributes overlays its computed keys
// for the same reason, and textFor was fixed for the same collision on "description".
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

// textFor returns the annotation body text.
//
// horopter.description is a Horopter extension, not an OTel semantic
// convention — no OTel convention names an annotation-body concept. It is
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
