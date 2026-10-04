package client_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"horopter.dev/horopter/internal/client"
)

// pages serves /v1/changes as len(pages) pages linked by next, and records each request.
func pages(t *testing.T, pages ...[]string) (*httptest.Server, *[]string) {
	t.Helper()
	var seen []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen = append(seen, r.URL.RequestURI())
		assert.Equal(t, "Bearer k", r.Header.Get("Authorization"))
		i := 0
		if c := r.URL.Query().Get("cursor"); c != "" {
			_, _ = fmt.Sscanf(c, "p%d", &i)
		}
		rows := make([]map[string]any, 0, len(pages[i]))
		for _, ref := range pages[i] {
			rows = append(rows, map[string]any{"ref": ref, "start": "2026-09-14T10:00:00Z",
				"status": "in_progress", "extra": "kept"})
		}
		body := map[string]any{"changes": rows}
		if i+1 < len(pages) {
			body["next"] = fmt.Sprintf("/v1/changes?from=x&cursor=p%d", i+1)
		}
		_ = json.NewEncoder(w).Encode(body)
	}))
	t.Cleanup(srv.Close)
	return srv, &seen
}

func refs(t *testing.T, c *client.Client, q client.ChangeQuery) ([]string, error) {
	t.Helper()
	var out []string
	for ch, err := range c.Changes(context.Background(), q) {
		if err != nil {
			return out, err
		}
		out = append(out, ch.Ref)
	}
	return out, nil
}

func window() client.ChangeQuery {
	return client.ChangeQuery{
		From: time.Date(2026, 9, 14, 0, 0, 0, 0, time.UTC),
		To:   time.Date(2026, 9, 15, 0, 0, 0, 0, time.UTC),
	}
}

func TestChangesFollowsNextToTheEnd(t *testing.T) {
	srv, seen := pages(t, []string{"a", "b"}, []string{"c"}, []string{"d"})
	c, err := client.New(srv.URL, "k")
	require.NoError(t, err)
	got, err := refs(t, c, window())
	require.NoError(t, err)
	assert.Equal(t, []string{"a", "b", "c", "d"}, got)
	assert.Len(t, *seen, 3)
}

func TestChangesStopsAtTheLimit(t *testing.T) {
	srv, seen := pages(t, []string{"a", "b"}, []string{"c"}, []string{"d"})
	c, err := client.New(srv.URL, "k")
	require.NoError(t, err)
	q := window()
	q.Limit = 3
	got, err := refs(t, c, q)
	require.NoError(t, err)
	assert.Equal(t, []string{"a", "b", "c"}, got)
	assert.Len(t, *seen, 2, "no page is fetched past the limit")
}

func TestRawKeepsEgresssOwnObject(t *testing.T) {
	srv, _ := pages(t, []string{"a"})
	c, err := client.New(srv.URL, "k")
	require.NoError(t, err)
	for ch, err := range c.Changes(context.Background(), window()) {
		require.NoError(t, err)
		assert.JSONEq(t,
			`{"ref":"a","start":"2026-09-14T10:00:00Z","status":"in_progress","extra":"kept"}`,
			string(ch.Raw))
	}
}

func TestAnEmptyPageWithNextIsAnError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"changes":[],"next":"/v1/changes?cursor=same"}`))
	}))
	t.Cleanup(srv.Close)
	c, err := client.New(srv.URL, "k")
	require.NoError(t, err)
	_, err = refs(t, c, window())
	require.Error(t, err)
	assert.Contains(t, err.Error(), "no rows")
}

func TestNextResolvesUnderAPathPrefix(t *testing.T) {
	var seen []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen = append(seen, r.URL.Path)
		if r.URL.Query().Get("cursor") == "" {
			_, _ = w.Write([]byte(`{"changes":[{"ref":"a","start":"2026-09-14T10:00:00Z"}],` +
				`"next":"/v1/changes?cursor=p1"}`))
			return
		}
		_, _ = w.Write([]byte(`{"changes":[{"ref":"b","start":"2026-09-14T10:00:00Z"}]}`))
	}))
	t.Cleanup(srv.Close)
	c, err := client.New(srv.URL+"/horopter", "k")
	require.NoError(t, err)
	got, err := refs(t, c, window())
	require.NoError(t, err)
	assert.Equal(t, []string{"a", "b"}, got)
	assert.Equal(t, []string{"/horopter/v1/changes", "/horopter/v1/changes"}, seen)
}

func TestAnErrorAnswerCarriesEgresssMessage(t *testing.T) {
	for status, want := range map[int]string{
		http.StatusUnauthorized: "a reader key",
		http.StatusBadRequest:   "attr.x is not filterable",
	} {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(status)
			_, _ = w.Write([]byte(`{"error":"attr.x is not filterable"}`))
		}))
		c, err := client.New(srv.URL, "k")
		require.NoError(t, err)
		_, err = refs(t, c, window())
		srv.Close()
		var he *client.HTTPError
		require.ErrorAs(t, err, &he)
		assert.Equal(t, status, he.Status)
		assert.Contains(t, err.Error(), want)
	}
}

func TestChangeReportsAMissingRef(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, "ci/none", r.URL.Query().Get("ref"))
		_, _ = w.Write([]byte(`{"changes":[]}`))
	}))
	t.Cleanup(srv.Close)
	c, err := client.New(srv.URL, "k")
	require.NoError(t, err)
	_, found, err := c.Change(context.Background(), "ci/none")
	require.NoError(t, err)
	assert.False(t, found)
}

func TestNewRefusesAnEmptyURLOrKey(t *testing.T) {
	_, err := client.New("", "k")
	require.Error(t, err)
	_, err = client.New("http://x", "")
	require.Error(t, err)
}
