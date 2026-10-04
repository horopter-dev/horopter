package egress

import (
	"net/http/httptest"
	"net/url"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestACursorRoundTripsItsKey(t *testing.T) {
	q := url.Values{"from": {"2026-09-14T09:00:00Z"}, "subject": {"acme/prod"}}
	token, err := encodeCursor([]string{"2026-09-14T10:00:00Z", "ci/dep-7"}, q)
	require.NoError(t, err)
	key, err := decodeCursor(token, q)
	require.NoError(t, err)
	assert.Equal(t, []string{"2026-09-14T10:00:00Z", "ci/dep-7"}, key)
}

func TestACursorIgnoresLimitAndItself(t *testing.T) {
	issued := url.Values{"subject": {"acme/prod"}, "limit": {"1000"}}
	token, err := encodeCursor([]string{"k"}, issued)
	require.NoError(t, err)
	replayed := url.Values{"subject": {"acme/prod"}, "limit": {"50"}, "cursor": {token}}
	_, err = decodeCursor(token, replayed)
	assert.NoError(t, err, "a different page size is the same question")
}

func TestACursorForADifferentQueryIsRefused(t *testing.T) {
	token, err := encodeCursor([]string{"k"}, url.Values{"subject": {"acme/prod"}})
	require.NoError(t, err)
	_, err = decodeCursor(token, url.Values{"subject": {"other/prod"}})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "cursor was issued for a different query")
}

func TestAnUndecodableCursorIsRefused(t *testing.T) {
	for _, token := range []string{"not base64!", "e30", "eyJrIjpbXX0"} {
		_, err := decodeCursor(token, url.Values{})
		assert.Error(t, err, token)
	}
}

func TestParameterOrderDoesNotChangeTheFingerprint(t *testing.T) {
	a := url.Values{"subject": {"x"}, "type": {"y"}}
	b := url.Values{"type": {"y"}, "subject": {"x"}}
	assert.Equal(t, queryFingerprint(a), queryFingerprint(b))
}

func TestNextURLRepeatsEveryParameterAndEscapes(t *testing.T) {
	r := httptest.NewRequest("GET", "/v1/changes?subject=a%26b%2Fc&limit=5&cursor=old", nil)
	next := nextURL(r, "tok")
	u, err := url.Parse(next)
	require.NoError(t, err)
	assert.Equal(t, "/v1/changes", u.Path)
	assert.Equal(t, "a&b/c", u.Query().Get("subject"))
	assert.Equal(t, "5", u.Query().Get("limit"))
	assert.Equal(t, "tok", u.Query().Get("cursor"))
}
