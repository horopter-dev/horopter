package egress

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"slices"
)

type cursorToken struct {
	K []string `json:"k"`
	Q string   `json:"q"`
}

var errBadCursor = errors.New("cursor is not a token egress issued; start again without it")

// limit only sizes a page, so a client may change it between pages.
func queryFingerprint(values url.Values) string {
	keys := make([]string, 0, len(values))
	for k := range values {
		if k != "cursor" && k != "limit" {
			keys = append(keys, k)
		}
	}
	slices.Sort(keys)
	h := sha256.New()
	for _, k := range keys {
		vs := slices.Clone(values[k])
		slices.Sort(vs)
		for _, v := range vs {
			// A hash.Hash never returns a write error.
			_, _ = fmt.Fprintf(h, "%s=%s\n", url.QueryEscape(k), url.QueryEscape(v))
		}
	}
	return base64.RawURLEncoding.EncodeToString(h.Sum(nil)[:16])
}

func encodeCursor(key []string, values url.Values) (string, error) {
	raw, err := json.Marshal(cursorToken{K: key, Q: queryFingerprint(values)})
	if err != nil {
		return "", fmt.Errorf("encode cursor: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(raw), nil
}

// Refuses a token issued for another query: stitching two queries' pages answers neither.
func decodeCursor(token string, values url.Values) ([]string, error) {
	raw, err := base64.RawURLEncoding.DecodeString(token)
	if err != nil {
		return nil, errBadCursor
	}
	var c cursorToken
	if json.Unmarshal(raw, &c) != nil || len(c.K) == 0 {
		return nil, errBadCursor
	}
	if c.Q != queryFingerprint(values) {
		return nil, errors.New(
			"cursor was issued for a different query; resend the parameters it was issued with")
	}
	return c.K, nil
}

func nextURL(r *http.Request, token string) string {
	values := r.URL.Query()
	values.Set("cursor", token)
	return r.URL.Path + "?" + values.Encode()
}
