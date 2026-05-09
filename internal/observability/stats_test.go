package observability

import (
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestStatsCollector_NilGatherer(t *testing.T) {
	sc := NewStatsCollector(nil, time.Now())
	_, err := sc.Summary()
	assert.Error(t, err)
}

func TestStatsCollector_AggregatesByStatusBucket(t *testing.T) {
	reg := prometheus.NewRegistry()
	m, err := NewMetrics(reg)
	require.NoError(t, err)

	// Mix of statuses across providers / models.
	m.ObserveRequest("openai", "gpt-4o", 200, 0.005)
	m.ObserveRequest("openai", "gpt-4o", 200, 0.012)
	m.ObserveRequest("openai", "gpt-4o", 401, 0.001)
	m.ObserveRequest("openai", "gpt-4o", 503, 0.020)
	m.ObserveRequest("openai", "gpt-4o", 502, 0.030)
	m.AddCost("openai", "gpt-4o", "ak_test", 12345)

	sc := NewStatsCollector(reg, time.Now())
	sum, err := sc.Summary()
	require.NoError(t, err)

	assert.Equal(t, uint64(5), sum.RequestsTotal)
	assert.Equal(t, uint64(1), sum.Errors4xx, "401 falls into 4xx bucket")
	assert.Equal(t, uint64(2), sum.Errors5xx, "503 + 502 → 5xx bucket")
	assert.Equal(t, int64(12345), sum.CostMicroTotal)
	assert.Greater(t, sum.P99LatencySeconds, 0.0,
		"with 5 samples we should get a non-zero p99 estimate")
}

func TestStatsCollector_CachesAcrossCalls(t *testing.T) {
	reg := prometheus.NewRegistry()
	m, err := NewMetrics(reg)
	require.NoError(t, err)

	m.ObserveRequest("openai", "gpt-4o", 200, 0.001)

	sc := NewStatsCollector(reg, time.Now())
	first, err := sc.Summary()
	require.NoError(t, err)

	// Bump again; the cache should hide the new sample for cacheTTL.
	m.ObserveRequest("openai", "gpt-4o", 200, 0.001)
	second, err := sc.Summary()
	require.NoError(t, err)

	assert.Equal(t, first.RequestsTotal, second.RequestsTotal,
		"cache hit must return identical projection")
}

func TestHistogramQuantile_LinearInterpolation(t *testing.T) {
	// Single bucket, 10 samples at upper=0.1: p50 should fall mid-
	// bucket → 0.05.
	h := mergedHistogram{
		sampleCount: 10,
		buckets:     []bucket{{upper: 0.1, count: 10}},
	}
	got := histogramQuantile(0.5, h)
	assert.InDelta(t, 0.05, got, 1e-9)
}

func TestHistogramQuantile_EmptyReturnsZero(t *testing.T) {
	assert.Equal(t, 0.0, histogramQuantile(0.99, mergedHistogram{}))
}

func TestStatsCollector_TopModelsSortedByRequests(t *testing.T) {
	reg := prometheus.NewRegistry()
	m, err := NewMetrics(reg)
	require.NoError(t, err)

	// Three models with different request counts.
	for i := 0; i < 5; i++ {
		m.ObserveRequest("openai", "gpt-4o", 200, 0.001)
	}
	for i := 0; i < 10; i++ {
		m.ObserveRequest("openai", "gpt-4o-mini", 200, 0.001)
	}
	for i := 0; i < 2; i++ {
		m.ObserveRequest("anthropic", "claude-3-5-sonnet", 200, 0.001)
	}
	m.AddTokens("openai", "gpt-4o-mini", 1000, 500)
	m.AddCost("openai", "gpt-4o-mini", "ak_x", 12345)

	sc := NewStatsCollector(reg, time.Now())
	sum, err := sc.Summary()
	require.NoError(t, err)

	require.Len(t, sum.TopModels, 3)
	assert.Equal(t, "gpt-4o-mini", sum.TopModels[0].Model, "highest requests must be first")
	assert.Equal(t, uint64(10), sum.TopModels[0].Requests)
	assert.Equal(t, uint64(1000), sum.TopModels[0].PromptTokens)
	assert.Equal(t, uint64(500), sum.TopModels[0].CompletionTokens)
	assert.Equal(t, int64(12345), sum.TopModels[0].CostMicro)

	assert.Equal(t, "gpt-4o", sum.TopModels[1].Model)
	assert.Equal(t, "claude-3-5-sonnet", sum.TopModels[2].Model)
}
