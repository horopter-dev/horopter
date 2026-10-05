package ingress_test

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"horopter.dev/horopter/internal/auth"
	"horopter.dev/horopter/internal/core"
	"horopter.dev/horopter/internal/store/postgres"
)

// seedRoleKeyIn issues a key of role in an existing tenant, with locus "test" and no subject
// pattern — the shape `horoctl admin keys issue --role <role> --tenant <id>` produces.
func seedRoleKeyIn(t *testing.T, s *postgres.Store, tenantID core.TenantID, role string) string {
	t.Helper()
	key, prefix, hash, err := auth.NewKey(rand.Reader)
	require.NoError(t, err)
	principalID, credentialID := "prn_test_"+prefix, "cred_test_"+prefix
	ctx := context.Background()
	_, err = s.Pool().Exec(ctx, `INSERT INTO principal (id, display_name) VALUES ($1, $1)`,
		principalID)
	require.NoError(t, err)
	t.Cleanup(func() { deleteRow(t, "principal", principalID) })
	_, err = s.Pool().Exec(ctx, `
		INSERT INTO credential (id, principal_id, type, key_prefix, key_hash, role,
		                        tenant_id, source, locus)
		VALUES ($1, $2, 'machine_key', $3, $4, $5, $6, $7, 'test')`,
		credentialID, principalID, prefix, hash, role, tenantID.String(), credentialSource)
	require.NoError(t, err)
	t.Cleanup(func() { deleteRow(t, "credential", credentialID) })
	return key
}

// refoldFixture is an ingress fixture with one change, github/d1, in the producer's tenant,
// and a tenant_admin key for that tenant.
type refoldFixture struct {
	ingressFixture
	adminKey string
}

func newRefoldFixture(t *testing.T) refoldFixture {
	t.Helper()
	f := newIngressFixture(t)
	rec := post(t, f.srv, f.producerKey, fmt.Sprintf(`{"type":"deployment.started",
		"subject":"acme/prod","occurred_at":"2026-09-14T10:00:00Z",
		"correlation_ref":"d1","idempotency_key":%q}`, idemKey(t, "1")))
	require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())
	return refoldFixture{
		ingressFixture: f,
		adminKey:       seedRoleKeyIn(t, f.store, f.producerTenant, "tenant_admin"),
	}
}

func refold(t *testing.T, h http.Handler, key, body string) *httptest.ResponseRecorder {
	t.Helper()
	return doPost(t, h, "/v1/admin/refold", "application/json", key, body)
}

func TestRefoldAnswersWithTheEventsAndTheFieldsItRepaired(t *testing.T) {
	f := newRefoldFixture(t)
	_, err := f.store.Pool().Exec(context.Background(),
		`UPDATE change SET title = 'wrong' WHERE tenant = $1 AND ref = 'github/d1'`,
		f.producerTenant.String())
	require.NoError(t, err)

	rec := refold(t, f.srv, f.adminKey, `{"ref":"github/d1"}`)

	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	assert.JSONEq(t, `{"ref":"github/d1","events":1,"altered":["title"]}`, rec.Body.String())
}

func TestRefoldOfAnUnchangedChangeAnswersAnEmptyList(t *testing.T) {
	f := newRefoldFixture(t)
	rec := refold(t, f.srv, f.adminKey, `{"ref":"github/d1"}`)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	assert.Contains(t, rec.Body.String(), `"altered":[]`, "never null, so jq length works")
}

func TestRefoldRefusesKeysWithoutAdmin(t *testing.T) {
	f := newRefoldFixture(t)
	reader := seedRoleKeyIn(t, f.store, f.producerTenant, "reader")
	for name, key := range map[string]string{"producer": f.producerKey, "reader": reader} {
		rec := refold(t, f.srv, key, `{"ref":"github/d1"}`)
		assert.Equal(t, http.StatusForbidden, rec.Code, "%s: %s", name, rec.Body.String())
	}
}

func TestATenantKeyMayNameOnlyItsOwnTenant(t *testing.T) {
	f := newRefoldFixture(t)
	own := refold(t, f.srv, f.adminKey,
		fmt.Sprintf(`{"ref":"github/d1","tenant":%q}`, f.producerTenant.String()))
	assert.Equal(t, http.StatusOK, own.Code, "naming its own tenant is fine: %s", own.Body.String())

	other := refold(t, f.srv, f.adminKey, `{"ref":"github/d1","tenant":"tnt_someone_else"}`)
	assert.Equal(t, http.StatusBadRequest, other.Code, other.Body.String())
}

func TestAGlobalKeyMustNameAnExistingTenant(t *testing.T) {
	f := newRefoldFixture(t)
	super := seedGlobalCredential(t, f.store, "superuser")

	none := refold(t, f.srv, super, `{"ref":"github/d1"}`)
	assert.Equal(t, http.StatusBadRequest, none.Code, none.Body.String())

	unknown := refold(t, f.srv, super, `{"ref":"github/d1","tenant":"tnt_never_existed"}`)
	assert.Equal(t, http.StatusNotFound, unknown.Code, unknown.Body.String())

	named := refold(t, f.srv, super,
		fmt.Sprintf(`{"ref":"github/d1","tenant":%q}`, f.producerTenant.String()))
	assert.Equal(t, http.StatusOK, named.Code, named.Body.String())
}

// TestRefoldWantsTheStoredRefForm: a producer ref has no locus, so it is refused as malformed
// with the form named — not answered 404, which would suggest the change does not exist.
func TestRefoldWantsTheStoredRefForm(t *testing.T) {
	f := newRefoldFixture(t)
	for _, body := range []string{`{"ref":"d1"}`, `{"ref":""}`, `{}`, `not json`} {
		rec := refold(t, f.srv, f.adminKey, body)
		assert.Equal(t, http.StatusBadRequest, rec.Code, "%s: %s", body, rec.Body.String())
	}
	rec := refold(t, f.srv, f.adminKey, `{"ref":"d1"}`)
	// Decoded, not read raw: encoding/json escapes < and > in the body as \u003c and \u003e.
	var answer struct {
		Error string `json:"error"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &answer))
	assert.Contains(t, answer.Error, "<locus>/<ref>", "ParseRef names the form")
	assert.Contains(t, answer.Error, "stored form", "the handler says where to find it")
}

func TestRefoldOfAnUnknownRefIsNotFound(t *testing.T) {
	f := newRefoldFixture(t)
	rec := refold(t, f.srv, f.adminKey, `{"ref":"github/never-existed"}`)
	assert.Equal(t, http.StatusNotFound, rec.Code, rec.Body.String())
	var answer struct {
		Error string `json:"error"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &answer))
	assert.True(t, strings.HasPrefix(answer.Error, "no events for ref"),
		"horo refold exits 3 only on this prefix (cmd/horo/refold.go refNotFound): %q", answer.Error)
}

func TestRefoldLogsTheRefAndWhatItAltered(t *testing.T) {
	f := newRefoldFixture(t)
	_, err := f.store.Pool().Exec(context.Background(),
		`UPDATE change SET title = 'wrong' WHERE tenant = $1 AND ref = 'github/d1'`,
		f.producerTenant.String())
	require.NoError(t, err)
	records := captureLogs(t)

	rec := refold(t, f.srv, f.adminKey, `{"ref":"github/d1"}`)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

	var line map[string]any
	for _, r := range requestLines(records()) {
		if r["ref"] == "github/d1" {
			line = r
		}
	}
	require.NotNil(t, line, "no request line carries the ref: %v", records())
	assert.InDelta(t, 1, line["refold_events"], 0)
	assert.Equal(t, []any{"title"}, line["refold_altered"])
}

// TestRefoldAnswerDecodes pins the answer's field names for clients.
func TestRefoldAnswerDecodes(t *testing.T) {
	f := newRefoldFixture(t)
	rec := refold(t, f.srv, f.adminKey, `{"ref":"github/d1"}`)
	var got struct {
		Ref     string   `json:"ref"`
		Events  int      `json:"events"`
		Altered []string `json:"altered"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &got))
	assert.Equal(t, "github/d1", got.Ref)
	assert.Equal(t, 1, got.Events)
}

// TestAFailedRefoldStillLogsItsRef: the request line is an admin action's audit record, so a
// re-fold that ended in 404 must still say which change it targeted.
func TestAFailedRefoldStillLogsItsRef(t *testing.T) {
	f := newRefoldFixture(t)
	records := captureLogs(t)

	rec := refold(t, f.srv, f.adminKey, `{"ref":"github/never-existed"}`)
	require.Equal(t, http.StatusNotFound, rec.Code, rec.Body.String())

	var found bool
	for _, r := range requestLines(records()) {
		found = found || r["ref"] == "github/never-existed"
	}
	assert.True(t, found, "the 404's request line must carry the ref: %v", records())
}

// TestAFailingStoreAnswers503ForARefold: a database blip during a re-fold is transient, and
// 503 is what tells a caller — horo refold among them — to retry. A 500 is not retried, and
// its body no longer says the request may be.
func TestAFailingStoreAnswers503ForARefold(t *testing.T) {
	f := newRefoldFixture(t)
	warm := refold(t, f.srv, f.adminKey, `{"ref":"github/d1"}`)
	require.Equal(t, http.StatusOK, warm.Code,
		"first call caches the credential: %s", warm.Body.String())
	f.store.Close()

	rec := refold(t, f.srv, f.adminKey, `{"ref":"github/d1"}`)
	assert.Equal(t, http.StatusServiceUnavailable, rec.Code, rec.Body.String())
	assert.Contains(t, rec.Body.String(), "may be retried")
}
