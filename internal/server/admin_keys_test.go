package server

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	"github.com/An-idd/x-beacon/internal/auth"
	"github.com/An-idd/x-beacon/internal/provider/registry"
	"github.com/An-idd/x-beacon/internal/storage"
)

// keysFixture sets up /admin/keys against a real Postgres pool
// (gated by XBEACON_TEST_DSN). The static authenticator carries a
// pre-seeded admin key with admin:webui scope; the Keystore writes
// to the same DB the static authenticator never sees, which is fine
// — the hand-off only happens on the e2e "use newly-created key"
// step where we rebuild the server with a Postgres authenticator.
type keysFixture struct {
	srv      *Server
	adminKey string
	pool     *storage.Pool
}

func newKeysFixture(t *testing.T) *keysFixture {
	t.Helper()
	dsn := os.Getenv("XBEACON_TEST_DSN")
	if dsn == "" {
		t.Skip("set XBEACON_TEST_DSN to run admin/keys integration tests")
	}
	require.NoError(t, storage.MigrateDown(dsn))
	require.NoError(t, storage.MigrateUp(dsn))

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	pool, err := storage.NewPool(ctx, storage.Config{DSN: dsn})
	require.NoError(t, err)
	t.Cleanup(pool.Close)

	const adminKey = "sk-admin-keys-test"
	authn, err := auth.NewStatic([]auth.StaticEntry{
		{ID: "admin", Name: "Admin", Secret: adminKey, Scopes: map[string][]string{"admin": {"webui"}}},
	})
	require.NoError(t, err)

	ks := auth.NewKeystore(pool, nil, zap.NewNop())

	reg := registry.NewEmpty()
	srv, err := New(Deps{
		Logger:         zap.NewNop(),
		Registry:       reg,
		Router:         newTestRouter(reg),
		Authn:          authn,
		Keystore:       ks,
		MetricsReg:     prometheus.NewRegistry(),
		MetricsEnabled: false,
	})
	require.NoError(t, err)

	return &keysFixture{srv: srv, adminKey: adminKey, pool: pool}
}

func (f *keysFixture) do(t *testing.T, method, path string, body []byte, key string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, path, bytes.NewReader(body))
	if key != "" {
		req.Header.Set("Authorization", "Bearer "+key)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	rec := httptest.NewRecorder()
	f.srv.Handler().ServeHTTP(rec, req)
	return rec
}

func TestAdminKeys_ListRequiresScope(t *testing.T) {
	f := newKeysFixture(t)
	rec := f.do(t, "GET", "/admin/keys", nil, "")
	assert.Equal(t, http.StatusUnauthorized, rec.Code)
}

func TestAdminKeys_CreateRejectsBadInput(t *testing.T) {
	f := newKeysFixture(t)

	cases := map[string][]byte{
		"missing label":      []byte(`{"scopes":["admin:webui"]}`),
		"missing scopes":     []byte(`{"label":"foo"}`),
		"empty scopes array": []byte(`{"label":"foo","scopes":[]}`),
		"malformed json":     []byte(`not json`),
		"scope no colon":     []byte(`{"label":"foo","scopes":["adminwebui"]}`),
	}
	for label, body := range cases {
		t.Run(label, func(t *testing.T) {
			rec := f.do(t, "POST", "/admin/keys", body, f.adminKey)
			assert.Equal(t, http.StatusBadRequest, rec.Code, "%s should be 400", label)
		})
	}
}

func TestAdminKeys_CreateRejectsBadScopeFormat(t *testing.T) {
	f := newKeysFixture(t)
	body := []byte(`{"label":"bad","scopes":["Admin:WebUI"]}`)
	rec := f.do(t, "POST", "/admin/keys", body, f.adminKey)
	assert.Equal(t, http.StatusBadRequest, rec.Code)
	assert.Contains(t, rec.Body.String(), "invalid")
}

// E2E: create a new key, use it as a Postgres-backed bearer (rebuilt
// server), revoke it, confirm the revoked key returns 401.
func TestAdminKeys_CreateUseRevoke401Cycle(t *testing.T) {
	f := newKeysFixture(t)

	// 1. Create.
	body := []byte(`{"label":"e2e-test","scopes":["admin:webui"]}`)
	rec := f.do(t, "POST", "/admin/keys", body, f.adminKey)
	require.Equal(t, http.StatusCreated, rec.Code, "body: %s", rec.Body.String())
	var created map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &created))
	newID, _ := created["id"].(string)
	newSecret, _ := created["key"].(string)
	require.NotEmpty(t, newID)
	require.NotEmpty(t, newSecret)

	// 2. Rebuild a server with a Postgres-backed authenticator so the
	//    new key actually authenticates against the same DB.
	pgAuthn := auth.NewPostgres(f.pool)
	ks := auth.NewKeystore(f.pool, nil, zap.NewNop())
	reg := registry.NewEmpty()
	srv2, err := New(Deps{
		Logger:         zap.NewNop(),
		Registry:       reg,
		Router:         newTestRouter(reg),
		Authn:          pgAuthn,
		Keystore:       ks,
		MetricsReg:     prometheus.NewRegistry(),
		MetricsEnabled: false,
	})
	require.NoError(t, err)

	doSrv2 := func(method, path string, key string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(method, path, nil)
		if key != "" {
			req.Header.Set("Authorization", "Bearer "+key)
		}
		rec := httptest.NewRecorder()
		srv2.Handler().ServeHTTP(rec, req)
		return rec
	}

	// 3. New key authenticates and lists.
	rec = doSrv2("GET", "/admin/keys", newSecret)
	assert.Equal(t, http.StatusOK, rec.Code, "fresh key must authenticate")

	// 4. Revoke via the original key (admin still works).
	rec = doSrv2("POST", "/admin/keys/"+newID+"/revoke", newSecret)
	require.Equal(t, http.StatusOK, rec.Code, "self-revoke is allowed; body: %s", rec.Body.String())

	// 5. Same key now 401s.
	rec = doSrv2("GET", "/admin/keys", newSecret)
	assert.Equal(t, http.StatusUnauthorized, rec.Code, "revoked key must 401 on next use")
}

func TestAdminKeys_RevokeMissingReturns404(t *testing.T) {
	f := newKeysFixture(t)
	rec := f.do(t, "POST", "/admin/keys/does-not-exist/revoke", nil, f.adminKey)
	assert.Equal(t, http.StatusNotFound, rec.Code)
}
