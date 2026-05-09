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
	"github.com/An-idd/x-beacon/internal/observability"
	"github.com/An-idd/x-beacon/internal/provider/registry"
)

func newProvidersFixture(t *testing.T) (*Server, string, *observability.Metrics) {
	t.Helper()

	// Two providers, distinct ownership; bench fixture style.
	yaml := `
providers:
  - name: primary
    type: openai
    endpoint: http://127.0.0.1:9999
    api_key: sk-test
    models:
      exact: ["gpt-4o-mini"]
  - name: backup
    type: openai
    endpoint: http://127.0.0.1:9998
    api_key: sk-test
    models:
      exact: ["gpt-4o"]
`
	dir := t.TempDir()
	path := filepath.Join(dir, "providers.yaml")
	require.NoError(t, os.WriteFile(path, []byte(yaml), 0o600))
	reg, err := registry.Load(path)
	require.NoError(t, err)

	const adminKey = "sk-admin-providers-test"
	authn, err := auth.NewStatic([]auth.StaticEntry{
		{ID: "admin", Name: "Admin", Secret: adminKey, Scopes: map[string][]string{"admin": {"webui"}}},
	})
	require.NoError(t, err)

	metricsReg := prometheus.NewRegistry()
	metrics, err := observability.NewMetrics(metricsReg)
	require.NoError(t, err)

	srv, err := New(Deps{
		Logger:         zap.NewNop(),
		Registry:       reg,
		Router:         newTestRouter(reg),
		Authn:          authn,
		Metrics:        metrics,
		MetricsReg:     metricsReg,
		MetricsEnabled: false,
	})
	require.NoError(t, err)
	return srv, adminKey, metrics
}

func TestAdminProviders_RequiresScope(t *testing.T) {
	srv, _, _ := newProvidersFixture(t)
	req := httptest.NewRequest(http.MethodGet, "/admin/providers", nil)
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)
	assert.Equal(t, http.StatusUnauthorized, rec.Code)
}

func TestAdminProviders_ProjectsRegistryAndBreakerState(t *testing.T) {
	srv, key, metrics := newProvidersFixture(t)

	// Simulate breaker transitions and a failover.
	metrics.SetBreakerState("primary", 2) // open
	metrics.SetBreakerState("backup", 0)  // closed
	metrics.IncFailover("primary", "backup")
	metrics.IncFailover("primary", "backup")

	req := httptest.NewRequest(http.MethodGet, "/admin/providers", nil)
	req.Header.Set("Authorization", "Bearer "+key)
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)

	require.Equal(t, http.StatusOK, rec.Code)
	var got struct {
		Providers []struct {
			Name          string   `json:"name"`
			Models        []string `json:"models"`
			BreakerState  string   `json:"breaker_state"`
			FailoversFrom uint64   `json:"failovers_from"`
			FailoversTo   uint64   `json:"failovers_to"`
		} `json:"providers"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &got))
	require.Len(t, got.Providers, 2)

	primary := got.Providers[0]
	backup := got.Providers[1]
	assert.Equal(t, "primary", primary.Name)
	assert.Equal(t, "open", primary.BreakerState)
	assert.Equal(t, uint64(2), primary.FailoversFrom)
	assert.Equal(t, uint64(0), primary.FailoversTo)

	assert.Equal(t, "backup", backup.Name)
	assert.Equal(t, "closed", backup.BreakerState)
	assert.Equal(t, uint64(0), backup.FailoversFrom)
	assert.Equal(t, uint64(2), backup.FailoversTo)
}
