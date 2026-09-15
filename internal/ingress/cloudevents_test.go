package ingress

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

// TestIdempotencyKeyIsInjective pins the property the encoding exists for: distinct
// (source, id) pairs never collide. CloudEvents source is a URI-reference, so embedded
// slashes are ordinary, and a plain "source/id" join makes "github/repo" + "123"
// indistinguishable from "github" + "repo/123". The consequence of that collision is
// identical to the one the composite key exists to remove — a distinct event silently
// swallowed and answered 200 {"created": false} — so narrowing the collision class is not
// the same as closing it.
//
// The pairs below are chosen so that every one of them collides under a plain join, or
// under a length prefix with no delimiter, or under a colon join. Comparing every pair
// against every other is what makes the test a statement about the encoding rather than
// about four hand-picked examples.
func TestIdempotencyKeyIsInjective(t *testing.T) {
	pairs := []struct{ source, id string }{
		{"github/repo", "123"},
		{"github", "repo/123"},
		{"github/repo/123", ""},
		{"", "github/repo/123"},
		{"a", "1:b"},
		{"a:1", "b"},
		{"1", "0:x"},
		{"10", ":x"},
		{"/buildkite/pipelines/api", "build-7"},
		{"/buildkite/pipelines", "/api/build-7"},
	}

	seen := map[string]int{}
	for i, p := range pairs {
		key := idempotencyKey(p.source, p.id)
		if prev, clash := seen[key]; clash {
			t.Errorf("source %q + id %q collides with source %q + id %q on key %q; "+
				"two distinct events would share one idempotency key and the second "+
				"would be silently swallowed",
				p.source, p.id, pairs[prev].source, pairs[prev].id, key)
			continue
		}
		seen[key] = i
	}
	assert.Len(t, seen, len(pairs), "every distinct pair must produce a distinct key")

	// A NUL separator would be injective too, and is deliberately not used: PostgreSQL
	// TEXT cannot store 0x00, so such a key would fail at the INSERT instead of deduping.
	for _, p := range pairs {
		assert.NotContains(t, idempotencyKey(p.source, p.id), "\x00",
			"the key is stored in a TEXT column, which cannot hold a NUL byte")
	}
}
