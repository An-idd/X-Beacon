package observability

import (
	"errors"
	"sync"
	"time"

	"github.com/prometheus/client_golang/prometheus"
)

// CacheStats is the aggregate projection of the cache observability
// surface. Wire shape for /admin/stats/cache. Counts are
// process-uptime totals, same caveat as StatsSummary.
type CacheStats struct {
	Window string    `json:"window"`
	Since  time.Time `json:"since"`
	AsOf   time.Time `json:"as_of"`

	Exact    CacheLayerStats        `json:"exact"`
	Semantic SemanticCacheStats     `json:"semantic"`
}

// CacheLayerStats covers one of the two layers (exact or semantic
// stripped of similarity-specific fields). HitRate is computed at
// the projection layer so the WebUI doesn't need to know the
// "writes are the denominator" convention; nil when writes == 0
// to make the empty-state explicit.
type CacheLayerStats struct {
	Hits     uint64   `json:"hits"`
	Writes   uint64   `json:"writes"`
	HitRate  *float64 `json:"hit_rate,omitempty"` // hits / (hits + writes - hits) … see comment
}

// SemanticCacheStats extends CacheLayerStats with the threshold
// gauge + a coarse similarity histogram so the WebUI can overlay
// "where the cutoff lives" against "what near-miss similarities
// actually look like".
type SemanticCacheStats struct {
	CacheLayerStats
	Threshold        float64           `json:"threshold"`
	SimilarityBuckets []SimilarityBucket `json:"similarity_buckets"`
}

// SimilarityBucket is one entry of a coarse cumulative histogram.
// Mirrors Prometheus's bucket semantics: `count` is the number of
// samples ≤ `le`. The frontend turns this into a delta-bar mini
// histogram by subtracting consecutive counts.
type SimilarityBucket struct {
	LE    float64 `json:"le"`
	Count uint64  `json:"count"`
}

// CacheStatsCollector projects cache metrics from a Prometheus
// registry, with a small TTL cache to absorb WebUI polling.
type CacheStatsCollector struct {
	gatherer  prometheus.Gatherer
	startedAt time.Time
	cacheTTL  time.Duration

	mu       sync.Mutex
	cached   *CacheStats
	cachedAt time.Time
}

// NewCacheStatsCollector builds a collector reading from gatherer.
// startedAt feeds the `since` field — pass the same value the
// stats summary endpoint uses (Metrics.StartedAt()).
func NewCacheStatsCollector(gatherer prometheus.Gatherer, startedAt time.Time) *CacheStatsCollector {
	return &CacheStatsCollector{
		gatherer:  gatherer,
		startedAt: startedAt,
		cacheTTL:  5 * time.Second,
	}
}

// Stats returns the latest cache projection. 5s TTL cache.
func (c *CacheStatsCollector) Stats() (*CacheStats, error) {
	if c == nil || c.gatherer == nil {
		return nil, errors.New("observability: cache stats collector has no gatherer")
	}
	now := time.Now()

	c.mu.Lock()
	if c.cached != nil && now.Sub(c.cachedAt) < c.cacheTTL {
		out := *c.cached
		c.mu.Unlock()
		return &out, nil
	}
	c.mu.Unlock()

	families, err := c.gatherer.Gather()
	if err != nil {
		return nil, err
	}

	out := CacheStats{
		Window: "process_uptime",
		Since:  c.startedAt,
		AsOf:   now,
	}

	for _, f := range families {
		switch f.GetName() {
		case "gateway_cache_hits_total":
			for _, m := range f.GetMetric() {
				v := uint64(m.GetCounter().GetValue())
				switch labelValue(m, "type") {
				case "exact":
					out.Exact.Hits += v
				case "semantic":
					out.Semantic.Hits += v
				}
			}
		case "gateway_cache_writes_total":
			for _, m := range f.GetMetric() {
				v := uint64(m.GetCounter().GetValue())
				switch labelValue(m, "type") {
				case "exact":
					out.Exact.Writes += v
				case "semantic":
					out.Semantic.Writes += v
				}
			}
		case "gateway_cache_semantic_threshold":
			for _, m := range f.GetMetric() {
				out.Semantic.Threshold = m.GetGauge().GetValue()
			}
		case "gateway_cache_semantic_similarity":
			for _, m := range f.GetMetric() {
				h := m.GetHistogram()
				if h == nil {
					continue
				}
				for _, b := range h.GetBucket() {
					out.Semantic.SimilarityBuckets = append(out.Semantic.SimilarityBuckets, SimilarityBucket{
						LE:    b.GetUpperBound(),
						Count: b.GetCumulativeCount(),
					})
				}
			}
		}
	}

	out.Exact.HitRate = computeHitRate(out.Exact.Hits, out.Exact.Writes)
	out.Semantic.HitRate = computeHitRate(out.Semantic.Hits, out.Semantic.Writes)

	c.mu.Lock()
	cp := out
	c.cached = &cp
	c.cachedAt = now
	c.mu.Unlock()
	return &out, nil
}

// computeHitRate returns hits / (hits + writes_on_miss). We don't
// have a "lookups total" counter; writes happen only on misses
// (anti-pollution gate elsewhere), so writes ≈ misses-that-bothered-
// caching. hits / (hits + writes) understates the true hit rate
// when miss responses fail the anti-pollution gate, but it's the
// closest signal we can compute without a new collector.
//
// Returns nil for "no data" so the WebUI shows a dash, not a fake 0%.
func computeHitRate(hits, writes uint64) *float64 {
	denom := hits + writes
	if denom == 0 {
		return nil
	}
	r := float64(hits) / float64(denom)
	return &r
}
