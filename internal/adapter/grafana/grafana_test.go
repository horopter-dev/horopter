package grafana_test

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/horopter-dev/horopter/internal/adapter/grafana"
	"github.com/horopter-dev/horopter/internal/core"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

var (
	start = time.Date(2026, 9, 14, 10, 0, 0, 0, time.UTC) // 1789380000000 ms
	now   = time.Date(2026, 9, 14, 10, 30, 0, 0, time.UTC)
)

func TestRenderClosedChangeIsARegion(t *testing.T) {
	end := start.Add(20 * time.Minute)
	out, err := grafana.New().Render([]core.Change{{
		Ref: "dep-7", Subject: "acme/prod", Type: "deployment", Title: "API v2.5.0",
		Start: start, End: &end, Status: core.StatusCompleted,
		Attributes: map[string]any{"service.version": "2.5.0"},
	}}, now)
	require.NoError(t, err)

	anns := out.([]grafana.Annotation)
	require.Len(t, anns, 1)
	assert.Equal(t, start.UnixMilli(), anns[0].Time)
	assert.Equal(t, end.UnixMilli(), anns[0].TimeEnd)
	assert.Equal(t, "API v2.5.0", anns[0].Title)
	assert.Contains(t, anns[0].Tags, "service.version:2.5.0",
		"attributes are flattened at render time, never at write time")
	assert.Contains(t, anns[0].Tags, "status:completed")
}

func TestRenderOpenChangeEndsAtNow(t *testing.T) {
	out, err := grafana.New().Render([]core.Change{{
		Ref: "dep-8", Start: start, End: nil, Status: core.StatusInProgress,
	}}, now)
	require.NoError(t, err)
	anns := out.([]grafana.Annotation)
	assert.Equal(t, now.UnixMilli(), anns[0].TimeEnd,
		"an in-flight change renders as a region growing to now")
}

func TestRenderPointHasEqualTimes(t *testing.T) {
	out, err := grafana.New().Render([]core.Change{{
		Ref: "cfg-1", Start: start, End: &start, Status: core.StatusOccurred,
	}}, now)
	require.NoError(t, err)
	anns := out.([]grafana.Annotation)
	assert.Equal(t, anns[0].Time, anns[0].TimeEnd, "a point must not render as a region")
}

// Golden test: the exact bytes Grafana receives. If this changes, a vendor
// concept has moved.
func TestRenderGoldenJSON(t *testing.T) {
	end := start.Add(20 * time.Minute)
	out, err := grafana.New().Render([]core.Change{{
		Ref: "dep-7", Subject: "acme/prod", Type: "deployment", Title: "API v2.5.0",
		Start: start, End: &end, Status: core.StatusCompleted,
		Attributes: map[string]any{
			"service.version":    "2.5.0",
			"cicd.pipeline.name": "deploy-api",
		},
	}}, now)
	require.NoError(t, err)

	b, err := json.Marshal(out)
	require.NoError(t, err)
	assert.JSONEq(t, `[{
		"time": 1789380000000,
		"timeEnd": 1789381200000,
		"title": "API v2.5.0",
		"tags": [
			"cicd.pipeline.name:deploy-api",
			"service.version:2.5.0",
			"status:completed",
			"subject:acme/prod"
		],
		"text": ""
	}]`, string(b))
}

// TestTextForReadsNamespacedDescriptionOnly pins that
// horopter.description populates the annotation body, and that an
// unnamespaced "description" attribute — an invented key the OTel semantic
// conventions do not define — no longer does, even when
// both are present on the same change.
func TestTextForReadsNamespacedDescriptionOnly(t *testing.T) {
	out, err := grafana.New().Render([]core.Change{{
		Ref: "dep-11", Start: start, End: &start, Status: core.StatusOccurred,
		Attributes: map[string]any{
			"description":          "unnamespaced, must be ignored",
			"horopter.description": "namespaced, must populate the annotation",
		},
	}}, now)
	require.NoError(t, err)

	anns := out.([]grafana.Annotation)
	assert.Equal(t, "namespaced, must populate the annotation", anns[0].Text)
}

// TestTextForIsEmptyWithoutNamespacedDescription is the other half of the
// same guard: a bare "description" attribute, with no namespaced sibling,
// must render an empty annotation body rather than leaking into it.
func TestTextForIsEmptyWithoutNamespacedDescription(t *testing.T) {
	out, err := grafana.New().Render([]core.Change{{
		Ref: "dep-12", Start: start, End: &start, Status: core.StatusOccurred,
		Attributes: map[string]any{"description": "must not leak into text"},
	}}, now)
	require.NoError(t, err)

	anns := out.([]grafana.Annotation)
	assert.Empty(t, anns[0].Text)
}

// Render(nil, now) must marshal to a JSON array, never null: Grafana's
// JSON-datasource contract expects an array, and a bare null can break panels.
func TestRenderNilChangesMarshalsToEmptyArray(t *testing.T) {
	out, err := grafana.New().Render(nil, now)
	require.NoError(t, err)

	b, err := json.Marshal(out)
	require.NoError(t, err)
	assert.JSONEq(t, `[]`, string(b))
}

// A producer attribute named "status" or "subject" must not produce a second tag under
// the same prefix: Grafana tags are a flat list, so a reader given both has no way to
// tell which is the change's own status. The change's value wins, matching how the OTLP
// adapter overlays its computed keys.
func TestTagsForOverlaysComputedTagsOverCollidingAttributes(t *testing.T) {
	out, err := grafana.New().Render([]core.Change{{
		Tenant: "acme", Ref: "dep-12", Start: start, End: &start,
		Status: core.StatusCompleted, Subject: "acme/prod",
		Attributes: map[string]any{
			"status":          "eu-west-1a",
			"subject":         "not-the-subject",
			"service.version": "2.5.0",
		},
	}}, now)
	require.NoError(t, err)

	tags := out.([]grafana.Annotation)[0].Tags
	assert.Equal(t, []string{
		"service.version:2.5.0", "status:completed", "subject:acme/prod",
	}, tags, "one tag per key, with the change's own value winning the collision")
}
