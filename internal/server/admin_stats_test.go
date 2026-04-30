package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	"github.com/An-idd/x-beacon/internal/auth"
	"github.com/An-idd/x-beacon/internal/observability"
	"github.com/An-idd/x-beacon/internal/provider/registry"
)

// statsFixture builds an admin server with /admin/stats wired against
// a fresh Prometheus registry + Metrics bundle, so tests can record
// requests and read them back through the handler.
type statsFixture struct {
	srv      *Server
	adminKey string
	metrics  *observability.Metrics
}

func newStatsFixture(t *testing.T) *statsFixture {
	t.Helper()

	const adminKey = "sk-admin-stats-test"
	authn, err := auth.NewStatic([]auth.StaticEntry{
		{ID: "admin", Name: "Admin", Secret: adminKey, Scopes: map[string][]string{"admin": {"webui"}}},
	})
	require.NoError(t, err)

	reg := prometheus.NewRegistry()
	metrics, err := observability.NewMetrics(reg)
	require.NoError(t, err)

	stats := observability.NewStatsCollector(reg, metrics.StartedAt())

	srvReg := registry.NewEmpty()
	srv, err := New(Deps{
		Logger:         zap.NewNop(),
		Registry:       srvReg,
		Router:         newTestRouter(srvReg),
		Authn:          authn,
		Metrics:        metrics,
		Stats:          stats,
		MetricsReg:     reg,
		MetricsEnabled: false,
	})
	require.NoError(t, err)

	return &statsFixture{srv: srv, adminKey: adminKey, metrics: metrics}
}

func TestAdminStats_SummaryRequiresScope(t *testing.T) {
	f := newStatsFixture(t)
	req := httptest.NewRequest(http.MethodGet, "/admin/stats/summary", nil)
	rec := httptest.NewRecorder()
	f.srv.Handler().ServeHTTP(rec, req)
	assert.Equal(t, http.StatusUnauthorized, rec.Code)
}

func TestAdminStats_SummaryAggregates(t *testing.T) {
	f := newStatsFixture(t)
	f.metrics.ObserveRequest("openai", "gpt-4o", 200, 0.001)
	f.metrics.ObserveRequest("openai", "gpt-4o", 401, 0.001)
	f.metrics.ObserveRequest("openai", "gpt-4o", 503, 0.001)
	f.metrics.AddCost("openai", "gpt-4o", "ak_test", 99)

	req := httptest.NewRequest(http.MethodGet, "/admin/stats/summary", nil)
	req.Header.Set("Authorization", "Bearer "+f.adminKey)
	rec := httptest.NewRecorder()
	f.srv.Handler().ServeHTTP(rec, req)

	require.Equal(t, http.StatusOK, rec.Code)
	var got map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &got))
	assert.Equal(t, float64(3), got["requests_total"])
	assert.Equal(t, float64(1), got["errors_4xx"])
	assert.Equal(t, float64(1), got["errors_5xx"])
	assert.Equal(t, float64(99), got["cost_micro_total"])
	assert.NotEmpty(t, got["since"], "since should be populated from Metrics.StartedAt()")
}

func TestAdminStats_TimeseriesShape(t *testing.T) {
	f := newStatsFixture(t)
	f.metrics.ObserveRequest("openai", "gpt-4o", 200, 0.001)

	req := httptest.NewRequest(http.MethodGet, "/admin/stats/timeseries?metric=qps", nil)
	req.Header.Set("Authorization", "Bearer "+f.adminKey)
	rec := httptest.NewRecorder()
	f.srv.Handler().ServeHTTP(rec, req)

	require.Equal(t, http.StatusOK, rec.Code)
	var got map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &got))
	assert.Equal(t, "qps", got["metric"])
	assert.Equal(t, "1h", got["window"])
	assert.Equal(t, "1m", got["step"])

	pts, ok := got["points"].([]any)
	require.True(t, ok, "points should be an array")
	assert.Len(t, pts, 60, "always 60 buckets in v0.1")

	// Find the bucket with non-zero counts; recorded request should
	// land in the most recent bucket.
	var foundSuccess bool
	for _, p := range pts {
		m := p.(map[string]any)
		if m["success"].(float64) >= 1 {
			foundSuccess = true
			break
		}
	}
	assert.True(t, foundSuccess, "the recorded ObserveRequest must surface as a success count")
}

func TestAdminStats_CORSPreflightWithoutAuth(t *testing.T) {
	const adminKey = "sk-admin-cors"
	authn, err := auth.NewStatic([]auth.StaticEntry{
		{ID: "admin", Name: "Admin", Secret: adminKey, Scopes: map[string][]string{"admin": {"webui"}}},
	})
	require.NoError(t, err)

	reg := prometheus.NewRegistry()
	metrics, err := observability.NewMetrics(reg)
	require.NoError(t, err)
	stats := observability.NewStatsCollector(reg, metrics.StartedAt())
	srvReg := registry.NewEmpty()

	srv, err := New(Deps{
		Logger:           zap.NewNop(),
		Registry:         srvReg,
		Router:           newTestRouter(srvReg),
		Authn:            authn,
		Metrics:          metrics,
		Stats:            stats,
		MetricsReg:       reg,
		AdminCORSOrigins: []string{"https://admin.example.com"},
	})
	require.NoError(t, err)

	// Preflight: no Authorization header, allowed origin → 204 + echo.
	req := httptest.NewRequest(http.MethodOptions, "/admin/stats/summary", nil)
	req.Header.Set("Origin", "https://admin.example.com")
	req.Header.Set("Access-Control-Request-Method", "GET")
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)

	assert.Equal(t, http.StatusNoContent, rec.Code)
	assert.Equal(t, "https://admin.example.com", rec.Header().Get("Access-Control-Allow-Origin"))
}
