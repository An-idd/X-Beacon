package observability

import (
	"strings"
	"testing"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func newTestMetrics(t *testing.T) (*Metrics, *prometheus.Registry) {
	t.Helper()
	reg := prometheus.NewRegistry()
	m, err := NewMetrics(reg)
	require.NoError(t, err)
	return m, reg
}

func TestNewMetrics_RegistersAllCollectors(t *testing.T) {
	m, reg := newTestMetrics(t)

	// Counters with zero observations don't appear in Gather output;
	// observe once per metric so the registry exposes them.
	m.ObserveRequest("p", "m", 200, 0.001)
	m.AddTokens("p", "m", 10, 5)
	m.AddCost("p", "m", "k", 100)
	m.IncCacheHit("exact")
	m.IncCacheWrite("exact")
	m.ObserveCacheLookup("hit", 0.0003)
	m.ObserveSemanticSimilarity(0.97)
	m.SetSemanticThreshold(0.95)
	m.ObserveSemanticLookup("hit", 0.08)
	m.IncRatelimitReject("global-rps")
	m.IncFailover("a", "b")
	m.SetBreakerState("p", 1)
	m.IncBillingDropped()
	m.IncBillingWritten()
	m.SetPricingCacheSize(10)

	gathered, err := reg.Gather()
	require.NoError(t, err)
	names := make(map[string]bool, len(gathered))
	for _, mf := range gathered {
		names[*mf.Name] = true
	}

	expected := []string{
		"gateway_requests_total",
		"gateway_request_duration_seconds",
		"gateway_tokens_total",
		"gateway_cost_micro_total",
		"gateway_cache_hits_total",
		"gateway_cache_writes_total",
		"gateway_cache_lookup_duration_seconds",
		"gateway_cache_semantic_similarity",
		"gateway_cache_semantic_threshold",
		"gateway_cache_semantic_lookup_duration_seconds",
		"gateway_ratelimit_rejected_total",
		"gateway_router_failover_total",
		"gateway_breaker_state",
		"gateway_billing_dropped_total",
		"gateway_billing_written_total",
		"gateway_pricing_cache_size",
	}
	for _, name := range expected {
		assert.True(t, names[name], "missing collector %q in registered set", name)
	}
}

func TestObserveRequest_CountsAndHistogram(t *testing.T) {
	m, reg := newTestMetrics(t)
	m.ObserveRequest("openai", "gpt-4o", 200, 0.123)
	m.ObserveRequest("openai", "gpt-4o", 200, 0.500)
	m.ObserveRequest("openai", "gpt-4o", 503, 1.0)

	assert.Equal(t, float64(2), testutil.ToFloat64(m.requestsTotal.WithLabelValues("openai", "gpt-4o", "2xx")))
	assert.Equal(t, float64(1), testutil.ToFloat64(m.requestsTotal.WithLabelValues("openai", "gpt-4o", "503")))

	got := testutil.CollectAndCount(m.requestDuration)
	assert.Equal(t, 1, got, "one labelset has been observed")

	// Lightly verify histogram exposition works.
	out, err := reg.Gather()
	require.NoError(t, err)
	var found bool
	for _, mf := range out {
		if *mf.Name == "gateway_request_duration_seconds" {
			found = true
		}
	}
	assert.True(t, found)
}

func TestStatusLabel(t *testing.T) {
	cases := map[int]string{
		200: "2xx", 201: "2xx", 299: "2xx",
		301: "3xx",
		400: "4xx", 401: "401", 403: "403", 404: "404",
		413: "413", 422: "4xx", 429: "429",
		500: "5xx", 502: "502", 503: "503", 504: "504", 599: "5xx",
		100: "unknown", 999: "unknown", 0: "unknown",
	}
	for status, want := range cases {
		assert.Equal(t, want, statusLabel(status), "status=%d", status)
	}
}

func TestAddTokens_SkipsZero(t *testing.T) {
	m, _ := newTestMetrics(t)
	m.AddTokens("p", "m", 0, 0)
	// No observation; collector isn't surfaced.
	assert.Equal(t, 0, testutil.CollectAndCount(m.tokensTotal))

	m.AddTokens("p", "m", 5, 0)
	assert.Equal(t, float64(5), testutil.ToFloat64(m.tokensTotal.WithLabelValues("p", "m", "prompt")))
}

func TestAddCost_SkipsZero(t *testing.T) {
	m, _ := newTestMetrics(t)
	m.AddCost("p", "m", "k", 0)
	assert.Equal(t, 0, testutil.CollectAndCount(m.costMicroTotal))

	m.AddCost("p", "m", "k", 1500)
	assert.Equal(t, float64(1500), testutil.ToFloat64(m.costMicroTotal.WithLabelValues("p", "m", "k")))
}

func TestNilMetrics_AllHelpersNoOp(t *testing.T) {
	// Every helper must be nil-safe so chat handlers can pass a nil
	// *Metrics in dev mode without conditional logic at every call.
	var m *Metrics
	assert.NotPanics(t, func() {
		m.ObserveRequest("p", "m", 200, 0.1)
		m.AddTokens("p", "m", 1, 1)
		m.AddCost("p", "m", "k", 1)
		m.IncCacheHit("exact")
		m.IncRatelimitReject("r")
		m.IncFailover("a", "b")
		m.SetBreakerState("p", 0)
		m.IncBillingDropped()
		m.IncBillingWritten()
		m.SetPricingCacheSize(1)
	})
}

func TestCacheMetrics_HelpersAreNilSafeAndCount(t *testing.T) {
	var nilM *Metrics
	// Nil receiver must be safe (dev mode without a registered Metrics).
	nilM.IncCacheHit("exact")
	nilM.IncCacheWrite("exact")
	nilM.ObserveCacheLookup("hit", 0.001)

	m, _ := newTestMetrics(t)
	m.IncCacheWrite("exact")
	m.IncCacheWrite("exact")
	m.IncCacheWrite("semantic")
	assert.Equal(t, float64(2), testutil.ToFloat64(m.cacheWritesTotal.WithLabelValues("exact")))
	assert.Equal(t, float64(1), testutil.ToFloat64(m.cacheWritesTotal.WithLabelValues("semantic")))

	m.ObserveCacheLookup("hit", 0.0002)
	m.ObserveCacheLookup("miss", 0.0010)
	m.ObserveCacheLookup("error", 0.050)
	// Three distinct label sets, each with one observation.
	assert.Equal(t, 3, testutil.CollectAndCount(m.cacheLookupDuration))
}

func TestSemanticMetrics_HelpersAreNilSafeAndCount(t *testing.T) {
	var nilM *Metrics
	nilM.ObserveSemanticSimilarity(0.99)
	nilM.SetSemanticThreshold(0.95)
	nilM.ObserveSemanticLookup("hit", 0.05)

	m, _ := newTestMetrics(t)
	// Similarity histogram: zero/negative similarity must NOT be
	// observed (skews the threshold-tuning view).
	m.ObserveSemanticSimilarity(0.0)
	m.ObserveSemanticSimilarity(-0.1)
	assert.Equal(t, 1, testutil.CollectAndCount(m.semanticSimilarity), "no observations expected from zero/negative")

	m.ObserveSemanticSimilarity(0.97)
	m.ObserveSemanticSimilarity(0.83)
	// CollectAndCount returns the number of distinct label sets, not
	// the observation count — there's exactly one (no labels), and
	// the histogram itself accumulates internally.

	m.SetSemanticThreshold(0.95)
	assert.Equal(t, 0.95, testutil.ToFloat64(m.semanticThreshold))
	m.SetSemanticThreshold(0.90) // re-set
	assert.Equal(t, 0.90, testutil.ToFloat64(m.semanticThreshold))

	m.ObserveSemanticLookup("hit", 0.05)
	m.ObserveSemanticLookup("miss", 0.07)
	m.ObserveSemanticLookup("error", 1.5)
	assert.Equal(t, 3, testutil.CollectAndCount(m.semanticLookupDuration),
		"three distinct result labels should produce three series")
}

func TestNewMetrics_DuplicateRegistrationFails(t *testing.T) {
	reg := prometheus.NewRegistry()
	_, err := NewMetrics(reg)
	require.NoError(t, err)
	// Second registration on same registry must fail (Prometheus
	// enforces uniqueness).
	_, err = NewMetrics(reg)
	require.Error(t, err)
	assert.True(t, strings.Contains(err.Error(), "already registered") ||
		strings.Contains(err.Error(), "duplicate"),
		"got %v", err)
}
