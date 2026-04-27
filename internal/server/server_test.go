package server

import (
	"encoding/json"
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
	"github.com/An-idd/x-beacon/internal/provider/registry"
	"github.com/An-idd/x-beacon/internal/router"
)

func newTestRouter(reg *registry.Registry) *router.Router {
	return router.New(reg, router.DefaultPolicy(), zap.NewNop())
}

func newTestServer(t *testing.T, opts ...func(*Deps)) *Server {
	t.Helper()
	reg := registry.NewEmpty()
	deps := Deps{
		Logger:         zap.NewNop(),
		Registry:       reg,
		Router:         newTestRouter(reg),
		MetricsReg:     prometheus.NewRegistry(),
		MetricsEnabled: true,
		MetricsPath:    "/metrics",
	}
	for _, opt := range opts {
		opt(&deps)
	}
	srv, err := New(deps)
	require.NoError(t, err)
	return srv
}

func TestNew_RequiresLogger(t *testing.T) {
	reg := registry.NewEmpty()
	_, err := New(Deps{Registry: reg, Router: newTestRouter(reg)})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "Logger")
}

func TestNew_RequiresRegistry(t *testing.T) {
	_, err := New(Deps{Logger: zap.NewNop(), Router: newTestRouter(registry.NewEmpty())})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "Registry")
}

func TestNew_RequiresRouter(t *testing.T) {
	_, err := New(Deps{Logger: zap.NewNop(), Registry: registry.NewEmpty()})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "Router")
}

func TestNew_RequiresMetricsRegWhenEnabled(t *testing.T) {
	reg := registry.NewEmpty()
	_, err := New(Deps{
		Logger:         zap.NewNop(),
		Registry:       reg,
		Router:         newTestRouter(reg),
		MetricsEnabled: true,
	})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "MetricsReg")
}

func TestNew_AllowsDisabledMetrics(t *testing.T) {
	reg := registry.NewEmpty()
	srv, err := New(Deps{
		Logger:         zap.NewNop(),
		Registry:       reg,
		Router:         newTestRouter(reg),
		MetricsEnabled: false,
	})
	require.NoError(t, err)
	assert.NotNil(t, srv.Handler())
}

func TestHealthz(t *testing.T) {
	srv := newTestServer(t)

	req := httptest.NewRequest(http.MethodGet, "/healthz", nil)
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)

	assert.Equal(t, http.StatusOK, rec.Code)
	assert.Equal(t, "ok\n", rec.Body.String())
}

func TestModels_EmptyRegistry(t *testing.T) {
	srv := newTestServer(t)

	req := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)

	assert.Equal(t, http.StatusOK, rec.Code)
	assert.Equal(t, "application/json", rec.Header().Get("Content-Type"))

	var raw map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &raw))
	assert.Equal(t, "list", raw["object"])
	data, ok := raw["data"].([]any)
	require.True(t, ok, "data must be JSON array, not null")
	assert.Empty(t, data)
}

func TestModels_PopulatedRegistry(t *testing.T) {
	yaml := `
providers:
  - name: openai
    type: openai
    api_key: sk-x
    models:
      exact: ["gpt-4o", "gpt-4o-mini"]
`
	dir := t.TempDir()
	path := filepath.Join(dir, "providers.yaml")
	require.NoError(t, os.WriteFile(path, []byte(yaml), 0o600))

	reg, err := registry.Load(path)
	require.NoError(t, err)

	srv := newTestServer(t, func(d *Deps) { d.Registry = reg })

	req := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)

	require.Equal(t, http.StatusOK, rec.Code)

	var resp modelsResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	assert.Equal(t, "list", resp.Object)
	require.Len(t, resp.Data, 2)
	assert.Equal(t, "gpt-4o", resp.Data[0].ID)
	assert.Equal(t, "gpt-4o-mini", resp.Data[1].ID)

	// Internal Provider field must not leak.
	var raw map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &raw))
	first, _ := raw["data"].([]any)[0].(map[string]any)
	_, leaked := first["Provider"]
	assert.False(t, leaked, "internal Provider field leaked to /v1/models")
}

func TestMetrics_MountedWhenEnabled(t *testing.T) {
	srv := newTestServer(t)

	req := httptest.NewRequest(http.MethodGet, "/metrics", nil)
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)

	assert.Equal(t, http.StatusOK, rec.Code)
}

func TestMetrics_NotMountedWhenDisabled(t *testing.T) {
	reg := registry.NewEmpty()
	srv, err := New(Deps{
		Logger:         zap.NewNop(),
		Registry:       reg,
		Router:         newTestRouter(reg),
		MetricsEnabled: false,
	})
	require.NoError(t, err)

	req := httptest.NewRequest(http.MethodGet, "/metrics", nil)
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)

	assert.Equal(t, http.StatusNotFound, rec.Code)
}

func TestAuth_AppliedToV1(t *testing.T) {
	authn, err := auth.NewStatic([]auth.StaticEntry{{ID: "test", Secret: "sk-test"}})
	require.NoError(t, err)
	srv := newTestServer(t, func(d *Deps) { d.Authn = authn })

	// /v1/models without Authorization → 401
	req := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)
	assert.Equal(t, http.StatusUnauthorized, rec.Code)

	// /v1/models with valid Authorization → 200
	req = httptest.NewRequest(http.MethodGet, "/v1/models", nil)
	req.Header.Set("Authorization", "Bearer sk-test")
	rec = httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)
	assert.Equal(t, http.StatusOK, rec.Code)
}

func TestAuth_NotAppliedToHealthzOrMetrics(t *testing.T) {
	authn, err := auth.NewStatic([]auth.StaticEntry{{ID: "test", Secret: "sk-test"}})
	require.NoError(t, err)
	srv := newTestServer(t, func(d *Deps) { d.Authn = authn })

	for _, path := range []string{"/healthz", "/metrics"} {
		req := httptest.NewRequest(http.MethodGet, path, nil)
		rec := httptest.NewRecorder()
		srv.Handler().ServeHTTP(rec, req)
		assert.Equal(t, http.StatusOK, rec.Code, "path %s should not require auth", path)
	}
}

func TestAuth_NilAuthn_LeavesV1Open(t *testing.T) {
	// dev-mode boot: no auth.yaml → server logs warn and skips auth, but
	// /v1/* still serves so /v1/models works for smoke testing.
	srv := newTestServer(t) // Authn defaults to nil

	req := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)
	assert.Equal(t, http.StatusOK, rec.Code)
}
