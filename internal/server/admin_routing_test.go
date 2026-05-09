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
	"github.com/An-idd/x-beacon/internal/route"
)

func newRoutingFixture(t *testing.T, classifier route.Classifier) (*Server, string, *observability.Metrics) {
	t.Helper()
	const adminKey = "sk-admin-routing-test"
	authn, err := auth.NewStatic([]auth.StaticEntry{
		{ID: "admin", Name: "Admin", Secret: adminKey, Scopes: map[string][]string{"admin": {"webui"}}},
	})
	require.NoError(t, err)

	reg := prometheus.NewRegistry()
	metrics, err := observability.NewMetrics(reg)
	require.NoError(t, err)

	srvReg := registry.NewEmpty()
	srv, err := New(Deps{
		Logger:         zap.NewNop(),
		Registry:       srvReg,
		Router:         newTestRouter(srvReg),
		Authn:          authn,
		Classifier:     classifier,
		Metrics:        metrics,
		MetricsReg:     reg,
		MetricsEnabled: false,
	})
	require.NoError(t, err)
	return srv, adminKey, metrics
}

func TestAdminRouting_RequiresScope(t *testing.T) {
	srv, _, _ := newRoutingFixture(t, nil)
	req := httptest.NewRequest(http.MethodGet, "/admin/routing/rules", nil)
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)
	assert.Equal(t, http.StatusUnauthorized, rec.Code)
}

func TestAdminRouting_DisabledClassifierReportsEnabledFalse(t *testing.T) {
	srv, key, _ := newRoutingFixture(t, nil)
	req := httptest.NewRequest(http.MethodGet, "/admin/routing/rules", nil)
	req.Header.Set("Authorization", "Bearer "+key)
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)

	require.Equal(t, http.StatusOK, rec.Code)
	var got map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &got))
	assert.Equal(t, false, got["enabled"])
	rules, _ := got["rules"].([]any)
	assert.Empty(t, rules)
}

func TestAdminRouting_ProjectsRulesAndHits(t *testing.T) {
	rules := []route.Rule{
		{Name: "cheap-translate", RouteTo: "gpt-4o-mini",
			When: route.Condition{KeywordsAny: []string{"translate"}, MaxTokens: 1000}},
		{Name: "long-context", RouteTo: "claude-3-5-sonnet",
			When: route.Condition{MinTokens: 50000}},
	}
	classifier, err := route.NewRuleClassifier(rules, nil)
	require.NoError(t, err)

	srv, key, metrics := newRoutingFixture(t, classifier)

	// Bump the cheap-translate counter twice; long-context stays at 0.
	metrics.IncRouterDecision("gpt-4o", "gpt-4o-mini", "cheap-translate")
	metrics.IncRouterDecision("gpt-4o", "gpt-4o-mini", "cheap-translate")

	req := httptest.NewRequest(http.MethodGet, "/admin/routing/rules", nil)
	req.Header.Set("Authorization", "Bearer "+key)
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)

	require.Equal(t, http.StatusOK, rec.Code)
	var got struct {
		Enabled bool `json:"enabled"`
		Rules   []struct {
			Name    string   `json:"name"`
			RouteTo string   `json:"route_to"`
			Hits    uint64   `json:"hits"`
			When    struct{} `json:"when"`
		} `json:"rules"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &got))
	assert.True(t, got.Enabled)
	require.Len(t, got.Rules, 2)
	assert.Equal(t, "cheap-translate", got.Rules[0].Name)
	assert.Equal(t, uint64(2), got.Rules[0].Hits)
	assert.Equal(t, "long-context", got.Rules[1].Name)
	assert.Equal(t, uint64(0), got.Rules[1].Hits)
}
