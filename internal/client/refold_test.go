package client_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"horopter.dev/horopter/internal/client"
)

// refoldIngress answers the statuses given in order, then 200; it records each body.
func refoldIngress(t *testing.T, statuses ...int) (string, *[]map[string]string) {
	t.Helper()
	var bodies []map[string]string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, "/v1/admin/refold", r.URL.Path)
		var body map[string]string
		_ = json.NewDecoder(r.Body).Decode(&body)
		bodies = append(bodies, body)
		if i := len(bodies) - 1; i < len(statuses) {
			w.WriteHeader(statuses[i])
			_, _ = w.Write([]byte(`{"error":"from the fake"}`))
			return
		}
		_, _ = w.Write([]byte(`{"ref":"ci/d1","events":2,"altered":["status"]}`))
	}))
	t.Cleanup(srv.Close)
	return srv.URL, &bodies
}

func TestRefoldRetriesA503AndDecodesTheAnswer(t *testing.T) {
	url, bodies := refoldIngress(t, http.StatusServiceUnavailable)
	c, err := client.New(url, "k")
	require.NoError(t, err)
	got, err := c.Refold(context.Background(), "ci/d1", "", time.Now().Add(5*time.Second))
	require.NoError(t, err)
	assert.Equal(t, client.Refolded{Ref: "ci/d1", Events: 2, Altered: []string{"status"}}, got)
	require.Len(t, *bodies, 2)
	assert.Equal(t, map[string]string{"ref": "ci/d1"}, (*bodies)[0], "no tenant unless given")
}

func TestRefoldSendsTheTenantWhenGiven(t *testing.T) {
	url, bodies := refoldIngress(t)
	c, err := client.New(url, "k")
	require.NoError(t, err)
	_, err = c.Refold(context.Background(), "ci/d1", "acme", time.Now().Add(5*time.Second))
	require.NoError(t, err)
	assert.Equal(t, "acme", (*bodies)[0]["tenant"])
}

func TestRefoldDoesNotRetryANotFound(t *testing.T) {
	url, bodies := refoldIngress(t, http.StatusNotFound)
	c, err := client.New(url, "k")
	require.NoError(t, err)
	_, err = c.Refold(context.Background(), "ci/d1", "", time.Now().Add(5*time.Second))
	var he *client.HTTPError
	require.ErrorAs(t, err, &he)
	assert.Equal(t, http.StatusNotFound, he.Status)
	assert.Len(t, *bodies, 1)
}
