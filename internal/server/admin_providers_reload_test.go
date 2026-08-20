package server

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	"github.com/An-idd/x-beacon/internal/auth"
	"github.com/An-idd/x-beacon/internal/observability"
	"github.com/An-idd/x-beacon/internal/provider/registry"
)

const reloadYAMLv1 = `
providers:
  - name: alpha
    type: openai
    endpoint: http://127.0.0.1:9999
    api_key: sk-test
    models:
      exact: ["model-a"]
`

const reloadYAMLv2 = `
providers:
  - name: alpha
    type: openai
    endpoint: http://127.0.0.1:9999
    api_key: sk-test
    models:
      exact: ["model-a"]
  - name: beta
    type: openai
    endpoint: http://127.0.0.1:9998
    api_key: sk-test
    models:
      exact: ["model-b"]
`

// newReloadFixture spins a server whose registry came from a real temp
// providers.yaml, returning the file path so tests can rewrite it.
func newReloadFixture(t *testing.T) (*Server, string, string) {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "providers.yaml")
	require.NoError(t, os.WriteFile(path, []byte(reloadYAMLv1), 0o600))
	reg, err := registry.Load(path)
	require.NoError(t, err)

	const adminKey = "sk-admin-reload-test"
	authn, err := auth.NewStatic([]auth.StaticEntry{
		{ID: "admin", Name: "Admin", Secret: adminKey, Scopes: map[string][]string{"admin": {"webui"}}},
	})
	require.NoError(t, err)

	metricsReg := prometheus.NewRegistry()
	metrics, err := observability.NewMetrics(metricsReg)
	require.NoError(t, err)

	srv, err := New(Deps{
		Logger:        zap.NewNop(),
		Registry:      reg,
		Router:        newTestRouter(reg),
		Authn:         authn,
		Metrics:       metrics,
		MetricsReg:    metricsReg,
		ProvidersFile: path,
	})
	require.NoError(t, err)
	return srv, adminKey, path
}

func postReload(srv *Server, key string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodPost, "/admin/providers/reload", nil)
	req.Header.Set("Authorization", "Bearer "+key)
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)
	return rec
}

func TestAdminProvidersReload_SwapsTable(t *testing.T) {
	srv, key, path := newReloadFixture(t)

	// Before: only model-a is known.
	req := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
	req.Header.Set("Authorization", "Bearer "+key)
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)
	assert.NotContains(t, rec.Body.String(), "model-b")

	// Rewrite the file and reload.
	require.NoError(t, os.WriteFile(path, []byte(reloadYAMLv2), 0o600))
	rec = postReload(srv, key)
	require.Equal(t, http.StatusOK, rec.Code, "body: %s", rec.Body.String())
	assert.Contains(t, rec.Body.String(), `"reloaded":true`)
	assert.Contains(t, rec.Body.String(), "beta")

	// After: model-b resolves.
	req = httptest.NewRequest(http.MethodGet, "/v1/models", nil)
	req.Header.Set("Authorization", "Bearer "+key)
	rec = httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)
	assert.Contains(t, rec.Body.String(), "model-b")
}

func TestAdminProvidersReload_InvalidFileKeepsOldTable(t *testing.T) {
	srv, key, path := newReloadFixture(t)

	require.NoError(t, os.WriteFile(path, []byte("providers: [{name: broken}]"), 0o600))
	rec := postReload(srv, key)
	require.Equal(t, http.StatusBadRequest, rec.Code)
	assert.Contains(t, rec.Body.String(), "providers_file_invalid")

	// Old table still serves model-a.
	req := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
	req.Header.Set("Authorization", "Bearer "+key)
	rr := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rr, req)
	assert.Contains(t, rr.Body.String(), "model-a")
}

func TestAdminProvidersReload_RequiresAuth(t *testing.T) {
	srv, _, _ := newReloadFixture(t)
	req := httptest.NewRequest(http.MethodPost, "/admin/providers/reload", nil)
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)
	assert.Equal(t, http.StatusUnauthorized, rec.Code)
}
