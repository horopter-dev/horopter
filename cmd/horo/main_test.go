package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const changeA = `{"ref":"ci/dep-1","subject":"acme/prod","type":"deployment.started",` +
	`"title":"Deploy api","status":"completed","start":"2026-09-14T10:00:00Z",` +
	`"end":"2026-09-14T10:02:30Z","first_seen_at":"2026-09-14T10:00:00Z"}`

// egress fakes the read API: /v1/changes answers changes (one page), /v1/events answers events.
func egress(t *testing.T, changes, events string) string {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/changes":
			if r.URL.Query().Get("ref") == "ci/missing" {
				_, _ = w.Write([]byte(`{"changes":[]}`))
				return
			}
			_, _ = w.Write([]byte(`{"changes":[` + changes + `]}`))
		case "/v1/events":
			_, _ = w.Write([]byte(`{"events":[` + events + `]}`))
		}
	}))
	t.Cleanup(srv.Close)
	return srv.URL
}

func horo(t *testing.T, url string, tty bool, args ...string) (int, string, string) {
	t.Helper()
	env := map[string]string{"HOROPTER_EGRESS_URL": url, "HOROPTER_EGRESS_KEY": "k"}
	var out, errOut bytes.Buffer
	code := run(args, func(k string) string { return env[k] }, &out, &errOut, tty)
	return code, out.String(), errOut.String()
}

func TestChangesPrintsATableOnATerminal(t *testing.T) {
	code, out, _ := horo(t, egress(t, changeA, ""), true, "changes", "--utc")
	require.Equal(t, 0, code)
	assert.Contains(t, out, "START")
	assert.Contains(t, out, "ci/dep-1")
	assert.Contains(t, out, "2m30s")
	assert.Contains(t, out, "2026-09-14T10:00:00Z")
}

func TestChangesWritesJSONLInAPipe(t *testing.T) {
	code, out, _ := horo(t, egress(t, changeA, ""), false, "changes")
	require.Equal(t, 0, code)
	lines := strings.Split(strings.TrimSpace(out), "\n")
	require.Len(t, lines, 1)
	assert.JSONEq(t, changeA, lines[0], "egress's own object, unchanged")
}

func TestChangesJSONIsOneArray(t *testing.T) {
	code, out, _ := horo(t, egress(t, changeA, ""), false, "changes", "--output", "json")
	require.Equal(t, 0, code)
	var rows []map[string]any
	require.NoError(t, json.Unmarshal([]byte(out), &rows))
	assert.Len(t, rows, 1)
}

func TestAnEmptyResultIsNotAnError(t *testing.T) {
	url := egress(t, "", "")
	code, out, _ := horo(t, url, true, "changes")
	assert.Equal(t, 0, code)
	assert.Contains(t, out, "START")
	assert.Equal(t, 1, strings.Count(strings.TrimSpace(out), "\n")+1, "header only")
	code, out, _ = horo(t, url, false, "changes", "--output", "json")
	assert.Equal(t, 0, code)
	assert.JSONEq(t, `[]`, out)
	code, out, _ = horo(t, url, false, "changes", "--output", "jsonl")
	assert.Equal(t, 0, code)
	assert.Empty(t, out)
}

func TestSinceWithFromIsAUsageError(t *testing.T) {
	code, _, errOut := horo(t, egress(t, "", ""), true,
		"changes", "--since", "1h", "--from", "2026-09-14T00:00:00Z")
	assert.Equal(t, 2, code)
	assert.Contains(t, errOut, "--since")
}

func TestAMissingKeyNamesTheFlagAndTheVariable(t *testing.T) {
	var out, errOut bytes.Buffer
	code := run([]string{"changes"}, func(string) string { return "" }, &out, &errOut, true)
	assert.Equal(t, 2, code)
	assert.Contains(t, errOut.String(), "HOROPTER_EGRESS_URL")
}

func TestShowPrintsTheChangeAndItsEvents(t *testing.T) {
	ev := `{"id":"01J9","type":"deployment.started","actor":"ci-bot","title":"Deploy api",` +
		`"occurred_at":"2026-09-14T10:00:00Z"}`
	code, out, _ := horo(t, egress(t, changeA, ev), true, "show", "--utc", "ci/dep-1")
	require.Equal(t, 0, code)
	assert.Contains(t, out, "ci/dep-1")
	assert.Contains(t, out, "OCCURRED")
	assert.Contains(t, out, "01J9")
}

func TestShowOfAMissingRefExitsThree(t *testing.T) {
	code, _, errOut := horo(t, egress(t, "", ""), true, "show", "ci/missing")
	assert.Equal(t, 3, code)
	assert.Contains(t, errOut, "ci/missing")
}

func TestARefusedKeyExitsOne(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"error":"missing or invalid credential"}`))
	}))
	t.Cleanup(srv.Close)
	code, _, errOut := horo(t, srv.URL, true, "changes")
	assert.Equal(t, 1, code)
	assert.Contains(t, errOut, "reader key")
}

func TestAnUnknownCommandIsAUsageError(t *testing.T) {
	code, _, errOut := horo(t, egress(t, "", ""), true, "explode")
	assert.Equal(t, 2, code)
	assert.Contains(t, errOut, "changes")
}

// TestTableOutputNeutralisesControlCharacters: titles and subjects are producer-supplied, and a
// terminal executes escape sequences printed to it. The table replaces control characters; json
// and jsonl carry egress's object unchanged.
func TestTableOutputNeutralisesControlCharacters(t *testing.T) {
	hostile := `{"ref":"ci/x","subject":"acme/prod","type":"t.started",` +
		`"title":"a\u001b]52;c;cGF3bmVk\u0007b\nc\td","status":"in_progress",` +
		`"start":"2026-09-14T10:00:00Z","first_seen_at":"2026-09-14T10:00:00Z"}`
	code, out, _ := horo(t, egress(t, hostile, ""), true, "changes", "--utc")
	require.Equal(t, 0, code)
	assert.NotContains(t, out, "\x1b")
	assert.NotContains(t, out, "\x07")
	assert.Equal(t, 2, strings.Count(out, "\n"), "header and one row: no embedded line break")
	code, out, _ = horo(t, egress(t, hostile, ""), false, "changes")
	require.Equal(t, 0, code)
	assert.JSONEq(t, hostile, strings.TrimSpace(out), "jsonl is egress's object, unchanged")
}

// TestUsageNeverPrintsTheKey: flag's usage output prints a flag's default, the key comes from
// the environment, and CI logs stderr — so a mistyped flag would publish the key.
func TestUsageNeverPrintsTheKey(t *testing.T) {
	env := map[string]string{
		"HOROPTER_EGRESS_URL": "http://x", "HOROPTER_EGRESS_KEY": "horo_SECRET",
	}
	var out, errOut bytes.Buffer
	code := run([]string{"changes", "--bogus"}, func(k string) string { return env[k] },
		&out, &errOut, false)
	assert.Equal(t, 2, code)
	assert.Contains(t, errOut.String(), "-key")
	assert.NotContains(t, errOut.String(), "horo_SECRET")
}
