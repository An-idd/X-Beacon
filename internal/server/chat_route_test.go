package server

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/An-idd/x-beacon/internal/auth"
	"github.com/An-idd/x-beacon/internal/cache"
	"github.com/An-idd/x-beacon/internal/observability"
	"github.com/An-idd/x-beacon/internal/provider/registry"
	"github.com/An-idd/x-beacon/internal/route"
)

// twoUpstreamFixture spins up two distinct mock upstreams (each with a
// distinct response body) so smart-route tests can prove which model
// was actually called. registry exposes both as independent
// providers (one model each).
func twoUpstreamFixture(t *testing.T) (reg *registry.Registry, primaryHits, cheapHits *atomic.Int64) {
	t.Helper()
	primaryHits = &atomic.Int64{}
	cheapHits = &atomic.Int64{}

	primary := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		primaryHits.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"id":"primary","object":"chat.completion","created":1,"model":"primary-model","choices":[{"index":0,"message":{"role":"assistant","content":"PRIMARY"},"finish_reason":"stop"}],"usage":{"prompt_tokens":3,"completion_tokens":1,"total_tokens":4}}`)
	}))
	t.Cleanup(primary.Close)

	cheap := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		cheapHits.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"id":"cheap","object":"chat.completion","created":1,"model":"cheap-model","choices":[{"index":0,"message":{"role":"assistant","content":"CHEAP"},"finish_reason":"stop"}],"usage":{"prompt_tokens":3,"completion_tokens":1,"total_tokens":4}}`)
	}))
	t.Cleanup(cheap.Close)

	yaml := fmt.Sprintf(`
providers:
  - name: test-primary
    type: openai
    endpoint: %s
    api_key: sk-test
    models:
      exact: ["primary-model"]
  - name: test-cheap
    type: openai
    endpoint: %s
    api_key: sk-test
    models:
      exact: ["cheap-model"]
`, primary.URL, cheap.URL)
	dir := t.TempDir()
	path := filepath.Join(dir, "providers.yaml")
	require.NoError(t, os.WriteFile(path, []byte(yaml), 0o600))

	reg, err := registry.Load(path)
	require.NoError(t, err)
	return reg, primaryHits, cheapHits
}

// post helper — POST /v1/chat/completions with the given body.
func postChat(t *testing.T, srv *Server, body []byte) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)
	return rec
}

func TestChat_SmartRoute_RewritesModelAndContactsCheapUpstream(t *testing.T) {
	reg, primaryHits, cheapHits := twoUpstreamFixture(t)

	classifier, err := route.NewRuleClassifier([]route.Rule{
		{
			Name:    "translate-to-cheap",
			RouteTo: "cheap-model",
			When:    route.Condition{KeywordsAny: []string{"translate"}},
		},
	}, nil)
	require.NoError(t, err)

	srv := newTestServer(t, func(d *Deps) {
		d.Registry = reg
		d.Router = newTestRouter(reg)
		d.Classifier = classifier
	})

	// Client asks primary-model; rule reroutes to cheap-model.
	body := chatBody("primary-model", "please translate this sentence", false)
	rec := postChat(t, srv, body)
	require.Equal(t, http.StatusOK, rec.Code)

	assert.Equal(t, int64(0), primaryHits.Load(), "primary upstream MUST NOT be called when rerouted")
	assert.Equal(t, int64(1), cheapHits.Load(), "cheap upstream takes the rerouted call")

	assert.Equal(t, "translate-to-cheap", rec.Header().Get("X-X-Beacon-Route-Rule"))
	assert.Equal(t, "primary-model", rec.Header().Get("X-X-Beacon-Route-From"))
	assert.Equal(t, "cheap-model", rec.Header().Get("X-X-Beacon-Route-To"))

	// Response body's model field reflects what actually generated it.
	var resp map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	assert.Equal(t, "cheap-model", resp["model"])
	assert.Equal(t, "cheap", resp["id"])
}

func TestChat_SmartRoute_NonMatchingPassesThrough(t *testing.T) {
	reg, primaryHits, cheapHits := twoUpstreamFixture(t)

	classifier, err := route.NewRuleClassifier([]route.Rule{
		{
			Name:    "translate-to-cheap",
			RouteTo: "cheap-model",
			When:    route.Condition{KeywordsAny: []string{"translate"}},
		},
	}, nil)
	require.NoError(t, err)

	srv := newTestServer(t, func(d *Deps) {
		d.Registry = reg
		d.Router = newTestRouter(reg)
		d.Classifier = classifier
	})

	body := chatBody("primary-model", "explain quantum entanglement in detail", false)
	rec := postChat(t, srv, body)
	require.Equal(t, http.StatusOK, rec.Code)

	assert.Equal(t, int64(1), primaryHits.Load(), "no rule matched: primary takes the call")
	assert.Equal(t, int64(0), cheapHits.Load())
	assert.Empty(t, rec.Header().Get("X-X-Beacon-Route-Rule"),
		"no route headers when classifier returned an empty Decision")
}

// Choice A invariant: the cache key uses the routed model. A
// rerouted client and a directly-cheap-model client SHARE cache.
func TestChat_SmartRoute_CacheKeyUsesRoutedModel(t *testing.T) {
	reg, primaryHits, cheapHits := twoUpstreamFixture(t)

	mr := miniredis.RunT(t)
	client := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = client.Close() })
	exact := cache.NewRedisExact(client)

	classifier, err := route.NewRuleClassifier([]route.Rule{
		{Name: "translate", RouteTo: "cheap-model", When: route.Condition{KeywordsAny: []string{"translate"}}},
	}, nil)
	require.NoError(t, err)

	srv := newTestServer(t, func(d *Deps) {
		d.Registry = reg
		d.Router = newTestRouter(reg)
		d.Classifier = classifier
		d.Cache = exact
		d.CacheTTL = time.Minute
	})

	// 1) Client A asks primary-model → rerouted to cheap-model →
	//    miss → upstream cheap → write under (cheap-model, ...) key.
	rec1 := postChat(t, srv, chatBody("primary-model", "translate this", false))
	require.Equal(t, http.StatusOK, rec1.Code)
	assert.Equal(t, "miss", rec1.Header().Get("X-X-Beacon-Cache"))
	assert.Equal(t, int64(1), cheapHits.Load())

	// 2) Client B asks cheap-model directly with the same content →
	//    rule does NOT match (rule fires only on rerouted requests
	//    that asked for primary-model + had keyword), so req.Model
	//    stays cheap-model. Cache lookup uses (cheap-model, ...) →
	//    HIT.
	rec2 := postChat(t, srv, chatBody("cheap-model", "translate this", false))
	require.Equal(t, http.StatusOK, rec2.Code)
	assert.Equal(t, "hit", rec2.Header().Get("X-X-Beacon-Cache"),
		"explicit cheap-model request must hit the cache populated by the rerouted request (Choice A)")
	assert.Equal(t, int64(1), cheapHits.Load(), "cheap upstream still at 1 — no second call")
	assert.Equal(t, int64(0), primaryHits.Load())
}

// TestChat_SmartRoute_ScopeOptOut: an API key with smart_route:disable
// scope must bypass the classifier entirely and reach the model it
// asked for, even though the request would otherwise have matched a
// rule. Header marker "skip:scope" advertises the opt-out for debug.
func TestChat_SmartRoute_ScopeOptOut(t *testing.T) {
	reg, primaryHits, cheapHits := twoUpstreamFixture(t)

	classifier, err := route.NewRuleClassifier([]route.Rule{
		{Name: "translate", RouteTo: "cheap-model", When: route.Condition{KeywordsAny: []string{"translate"}}},
	}, nil)
	require.NoError(t, err)

	authn, err := auth.NewStatic([]auth.StaticEntry{{
		ID:     "control",
		Secret: "sk-control",
		Scopes: map[string][]string{"smart_route": {"disable"}},
	}})
	require.NoError(t, err)

	srv := newTestServer(t, func(d *Deps) {
		d.Registry = reg
		d.Router = newTestRouter(reg)
		d.Classifier = classifier
		d.Authn = authn
	})

	body := chatBody("primary-model", "please translate this sentence", false)
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer sk-control")
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)

	require.Equal(t, http.StatusOK, rec.Code)
	assert.Equal(t, int64(1), primaryHits.Load(),
		"opt-out client must reach the model they asked for")
	assert.Equal(t, int64(0), cheapHits.Load(),
		"classifier must not run for opt-out clients")
	assert.Equal(t, "skip:scope", rec.Header().Get("X-X-Beacon-Route-Rule"))
	assert.Empty(t, rec.Header().Get("X-X-Beacon-Route-To"))
}

// TestChat_SmartRoute_ScopeOptOutPreservesCacheIsolation: this is the
// payoff of Choice A. A rerouted client and an opt-out client with
// IDENTICAL prompt content must NOT share cache entries.
func TestChat_SmartRoute_ScopeOptOutPreservesCacheIsolation(t *testing.T) {
	reg, primaryHits, cheapHits := twoUpstreamFixture(t)

	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = rdb.Close() })
	exact := cache.NewRedisExact(rdb)

	classifier, err := route.NewRuleClassifier([]route.Rule{
		{Name: "translate", RouteTo: "cheap-model", When: route.Condition{KeywordsAny: []string{"translate"}}},
	}, nil)
	require.NoError(t, err)

	authn, err := auth.NewStatic([]auth.StaticEntry{
		{ID: "treat", Secret: "sk-treat"},
		{ID: "control", Secret: "sk-control",
			Scopes: map[string][]string{"smart_route": {"disable"}}},
	})
	require.NoError(t, err)

	srv := newTestServer(t, func(d *Deps) {
		d.Registry = reg
		d.Router = newTestRouter(reg)
		d.Classifier = classifier
		d.Cache = exact
		d.CacheTTL = time.Minute
		d.Authn = authn
	})

	post := func(t *testing.T, key string) *httptest.ResponseRecorder {
		t.Helper()
		body := chatBody("primary-model", "please translate this", false)
		req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", bytes.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Authorization", "Bearer "+key)
		rec := httptest.NewRecorder()
		srv.Handler().ServeHTTP(rec, req)
		return rec
	}

	// Treatment-arm client: rerouted to cheap. Cache key uses cheap-model.
	rec1 := post(t, "sk-treat")
	require.Equal(t, http.StatusOK, rec1.Code)
	assert.Equal(t, "miss", rec1.Header().Get("X-X-Beacon-Cache"))
	assert.Equal(t, int64(1), cheapHits.Load())
	assert.Equal(t, int64(0), primaryHits.Load())

	// Control-arm client: SAME content but opt-out. Choice A means
	// cache key uses primary-model — must MISS and contact the
	// primary upstream, NOT serve the rerouted answer.
	rec2 := post(t, "sk-control")
	require.Equal(t, http.StatusOK, rec2.Code)
	assert.Equal(t, "miss", rec2.Header().Get("X-X-Beacon-Cache"),
		"control-arm client must NOT hit the treatment-arm's cache (Choice A invariant)")
	assert.Equal(t, "skip:scope", rec2.Header().Get("X-X-Beacon-Route-Rule"))
	assert.Equal(t, int64(1), primaryHits.Load(),
		"control client served by the model they asked for")
	assert.Equal(t, int64(1), cheapHits.Load(), "cheap upstream untouched on second call")
}

// TestChat_SmartRoute_MetricsScrape verifies the Week 11 metrics:
// rerouted requests bump gateway_router_decision_total, scope-disabled
// requests bump gateway_router_bypass_total{reason="scope"}, and
// non-matching requests don't touch either.
func TestChat_SmartRoute_MetricsScrape(t *testing.T) {
	reg, _, _ := twoUpstreamFixture(t)

	classifier, err := route.NewRuleClassifier([]route.Rule{
		{Name: "translate", RouteTo: "cheap-model", When: route.Condition{KeywordsAny: []string{"translate"}}},
	}, nil)
	require.NoError(t, err)

	authn, err := auth.NewStatic([]auth.StaticEntry{
		{ID: "treat", Secret: "sk-treat"},
		{ID: "control", Secret: "sk-control",
			Scopes: map[string][]string{"smart_route": {"disable"}}},
	})
	require.NoError(t, err)

	metricsReg := prometheus.NewRegistry()
	gm, err := observability.NewMetrics(metricsReg)
	require.NoError(t, err)

	srv := newTestServer(t, func(d *Deps) {
		d.Registry = reg
		d.Router = newTestRouter(reg)
		d.Classifier = classifier
		d.Authn = authn
		d.Metrics = gm
		d.MetricsReg = metricsReg
	})

	post := func(t *testing.T, key, content string) {
		t.Helper()
		req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions",
			bytes.NewReader(chatBody("primary-model", content, false)))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Authorization", "Bearer "+key)
		rec := httptest.NewRecorder()
		srv.Handler().ServeHTTP(rec, req)
		require.Equal(t, http.StatusOK, rec.Code)
	}

	post(t, "sk-treat", "translate this") // → reroute counted
	post(t, "sk-treat", "translate again") // → another reroute
	post(t, "sk-treat", "explain physics") // → no rule matched, neither counter
	post(t, "sk-control", "translate this") // → bypass counted

	body := scrapeMetrics(t, srv)
	assert.Contains(t, body,
		`gateway_router_decision_total{from="primary-model",rule="translate",to="cheap-model"} 2`)
	assert.Contains(t, body,
		`gateway_router_bypass_total{reason="scope"} 1`)
}

func TestChat_NilClassifier_NoOp(t *testing.T) {
	reg, primaryHits, _ := twoUpstreamFixture(t)
	srv := newTestServer(t, func(d *Deps) {
		d.Registry = reg
		d.Router = newTestRouter(reg)
		// d.Classifier left nil
	})

	rec := postChat(t, srv, chatBody("primary-model", "translate this", false))
	require.Equal(t, http.StatusOK, rec.Code)
	assert.Equal(t, int64(1), primaryHits.Load())
	assert.Empty(t, rec.Header().Get("X-X-Beacon-Route-Rule"))
}
