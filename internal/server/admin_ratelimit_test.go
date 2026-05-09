package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	"github.com/An-idd/x-beacon/internal/auth"
	"github.com/An-idd/x-beacon/internal/config"
	"github.com/An-idd/x-beacon/internal/observability"
	"github.com/An-idd/x-beacon/internal/provider/registry"
)

func newRatelimitFixture(t *testing.T, rules []config.RateLimitRule) (*Server, string, *observability.Metrics) {
	t.Helper()
	const adminKey = "sk-admin-rl-test"
	authn, err := auth.NewStatic([]auth.StaticEntry{
		{ID: "admin", Name: "Admin", Secret: adminKey, Scopes: map[string][]string{"admin": {"webui"}}},
	})
	require.NoError(t, err)

	mreg := prometheus.NewRegistry()
	metrics, err := observability.NewMetrics(mreg)
	require.NoError(t, err)

	srvReg := registry.NewEmpty()
	srv, err := New(Deps{
		Logger:         zap.NewNop(),
		Registry:       srvReg,
		Router:         newTestRouter(srvReg),
		Authn:          authn,
		Metrics:        metrics,
		MetricsReg:     mreg,
		MetricsEnabled: false,
		RateLimitRules: rules,
	})
	require.NoError(t, err)
	return srv, adminKey, metrics
}

func TestAdminRatelimit_RequiresScope(t *testing.T) {
	srv, _, _ := newRatelimitFixture(t, nil)
	req := httptest.NewRequest(http.MethodGet, "/admin/ratelimit/rules", nil)
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)
	assert.Equal(t, http.StatusUnauthorized, rec.Code)
}

func TestAdminRatelimit_EmptyRulesEnabledFalse(t *testing.T) {
	srv, key, _ := newRatelimitFixture(t, nil)
	req := httptest.NewRequest(http.MethodGet, "/admin/ratelimit/rules", nil)
	req.Header.Set("Authorization", "Bearer "+key)
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)

	require.Equal(t, http.StatusOK, rec.Code)
	var got map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &got))
	assert.Equal(t, false, got["enabled"])
}

func TestAdminRatelimit_ProjectsRulesAndRejects(t *testing.T) {
	rules := []config.RateLimitRule{
		{Name: "global", Algorithm: "memory_bucket", Rate: "100/s", KeyBy: nil},
		{Name: "per_key", Algorithm: "redis_window", Window: time.Minute, Limit: 60, KeyBy: []string{"api_key"}},
	}
	srv, key, metrics := newRatelimitFixture(t, rules)

	metrics.IncRatelimitReject("global")
	metrics.IncRatelimitReject("global")
	metrics.IncRatelimitReject("per_key")

	req := httptest.NewRequest(http.MethodGet, "/admin/ratelimit/rules", nil)
	req.Header.Set("Authorization", "Bearer "+key)
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)

	require.Equal(t, http.StatusOK, rec.Code)
	var got struct {
		Enabled bool `json:"enabled"`
		Rules   []struct {
			Name      string   `json:"name"`
			Algorithm string   `json:"algorithm"`
			Rate      string   `json:"rate"`
			Window    string   `json:"window"`
			Limit     int      `json:"limit"`
			KeyBy     []string `json:"key_by"`
			Rejects   uint64   `json:"rejects"`
		} `json:"rules"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &got))
	assert.True(t, got.Enabled)
	require.Len(t, got.Rules, 2)

	assert.Equal(t, "global", got.Rules[0].Name)
	assert.Equal(t, "memory_bucket", got.Rules[0].Algorithm)
	assert.Equal(t, "100/s", got.Rules[0].Rate)
	assert.Equal(t, uint64(2), got.Rules[0].Rejects)

	assert.Equal(t, "per_key", got.Rules[1].Name)
	assert.Equal(t, "redis_window", got.Rules[1].Algorithm)
	assert.Equal(t, "1m0s", got.Rules[1].Window)
	assert.Equal(t, 60, got.Rules[1].Limit)
	assert.Equal(t, uint64(1), got.Rules[1].Rejects)
}
