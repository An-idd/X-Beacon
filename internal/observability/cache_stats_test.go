package observability

import (
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestCacheStatsCollector_NilGatherer(t *testing.T) {
	c := NewCacheStatsCollector(nil, time.Now())
	_, err := c.Stats()
	assert.Error(t, err)
}

func TestCacheStatsCollector_AggregatesByLayer(t *testing.T) {
	reg := prometheus.NewRegistry()
	m, err := NewMetrics(reg)
	require.NoError(t, err)

	m.IncCacheHit("exact")
	m.IncCacheHit("exact")
	m.IncCacheHit("semantic")
	m.IncCacheWrite("exact")
	m.IncCacheWrite("semantic")
	m.IncCacheWrite("semantic")
	m.SetSemanticThreshold(0.95)
	m.ObserveSemanticSimilarity(0.92)
	m.ObserveSemanticSimilarity(0.97)

	c := NewCacheStatsCollector(reg, time.Now())
	out, err := c.Stats()
	require.NoError(t, err)

	assert.Equal(t, uint64(2), out.Exact.Hits)
	assert.Equal(t, uint64(1), out.Exact.Writes)
	require.NotNil(t, out.Exact.HitRate)
	assert.InDelta(t, 2.0/3.0, *out.Exact.HitRate, 1e-9)

	assert.Equal(t, uint64(1), out.Semantic.Hits)
	assert.Equal(t, uint64(2), out.Semantic.Writes)
	assert.Equal(t, 0.95, out.Semantic.Threshold)
	assert.NotEmpty(t, out.Semantic.SimilarityBuckets,
		"semantic similarity histogram must have at least one bucket")
}

func TestCacheStatsCollector_HitRateNilOnEmpty(t *testing.T) {
	reg := prometheus.NewRegistry()
	_, err := NewMetrics(reg)
	require.NoError(t, err)

	c := NewCacheStatsCollector(reg, time.Now())
	out, err := c.Stats()
	require.NoError(t, err)
	assert.Nil(t, out.Exact.HitRate, "no hits/writes → hit_rate must be nil so UI shows '—'")
	assert.Nil(t, out.Semantic.HitRate)
}

func TestCacheStatsCollector_Caches5s(t *testing.T) {
	reg := prometheus.NewRegistry()
	m, err := NewMetrics(reg)
	require.NoError(t, err)

	m.IncCacheHit("exact")

	c := NewCacheStatsCollector(reg, time.Now())
	first, err := c.Stats()
	require.NoError(t, err)

	m.IncCacheHit("exact")
	second, err := c.Stats()
	require.NoError(t, err)
	assert.Equal(t, first.Exact.Hits, second.Exact.Hits,
		"cache hit must hide the second IncCacheHit until TTL expires")
}
