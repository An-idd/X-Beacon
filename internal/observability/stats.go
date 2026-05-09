package observability

import (
	"errors"
	"sort"
	"sync"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	dto "github.com/prometheus/client_model/go"
)

// StatsSummary is the aggregated dashboard projection. All counts are
// process-uptime totals, NOT a true 24h rolling window — the gateway
// doesn't carry a Prometheus query API and we explicitly chose not
// to embed one (see [docs/webui-backend-tasks.md] task 3 decision G).
//
// Window / Since fields together let the WebUI display "totals since
// 04-29 08:00" instead of misleading "today" copy.
type StatsSummary struct {
	Window            string         `json:"window"`
	Since             time.Time      `json:"since"`
	AsOf              time.Time      `json:"as_of"`
	RequestsTotal     uint64         `json:"requests_total"`
	Errors4xx         uint64         `json:"errors_4xx"`
	Errors5xx         uint64         `json:"errors_5xx"`
	CostMicroTotal    int64          `json:"cost_micro_total"`
	P99LatencySeconds float64        `json:"p99_latency_seconds"`
	// TopModels is the 10 most-used (by request count) (provider,
	// model) pairs since process start. Sorted descending by
	// requests; ties broken by cost. Empty until the process sees
	// any traffic. The list is intentionally short — Grafana is
	// where you go for cardinality > 10.
	TopModels []ModelBreakdown `json:"top_models"`
}

// ModelBreakdown is one (provider, model) row of the dashboard's
// per-model summary. Values are process-uptime totals, like the
// rest of StatsSummary.
type ModelBreakdown struct {
	Provider         string `json:"provider"`
	Model            string `json:"model"`
	Requests         uint64 `json:"requests"`
	PromptTokens     uint64 `json:"prompt_tokens"`
	CompletionTokens uint64 `json:"completion_tokens"`
	CostMicro        int64  `json:"cost_micro"`
}

const topModelsLimit = 10

// modelKey is the per-(provider, model) accumulator key used by
// Summary's top-models computation. Kept at package scope so the
// topModels helper can be defined separately for testability.
type modelKey struct{ provider, model string }

// StatsCollector reads aggregated values from a Prometheus registry on
// demand. Computed values are cached for cacheTTL to keep the admin
// surface cheap when polled at WebUI cadence (~5–30s).
//
// The collector is read-only with respect to the registry; it does not
// register or own any new collectors. ObserveRequest already feeds
// the gateway's *Metrics — StatsCollector just projects.
type StatsCollector struct {
	gatherer prometheus.Gatherer
	startedAt time.Time
	cacheTTL  time.Duration

	mu       sync.Mutex
	cached   *StatsSummary
	cachedAt time.Time
}

// NewStatsCollector wires a gatherer (typically the *prometheus.Registry
// used by /metrics) with the process startedAt. Cache TTL is 5s — short
// enough to feel real-time, long enough to absorb a refresh-spam WebUI.
func NewStatsCollector(gatherer prometheus.Gatherer, startedAt time.Time) *StatsCollector {
	return &StatsCollector{
		gatherer:  gatherer,
		startedAt: startedAt,
		cacheTTL:  5 * time.Second,
	}
}

// Summary returns the latest projection. The 5s cache means polling
// at any rate above ~12 rpm collapses to one Gather() per window.
func (s *StatsCollector) Summary() (*StatsSummary, error) {
	if s == nil || s.gatherer == nil {
		return nil, errors.New("observability: stats collector has no gatherer")
	}
	now := time.Now()

	s.mu.Lock()
	if s.cached != nil && now.Sub(s.cachedAt) < s.cacheTTL {
		out := *s.cached
		s.mu.Unlock()
		return &out, nil
	}
	s.mu.Unlock()

	families, err := s.gatherer.Gather()
	if err != nil {
		return nil, err
	}

	summary := StatsSummary{
		Window: "process_uptime",
		Since:  s.startedAt,
		AsOf:   now,
	}

	// Per-(provider, model) accumulator for the top-models list.
	perModel := map[modelKey]*ModelBreakdown{}
	getOrInit := func(p, mname string) *ModelBreakdown {
		k := modelKey{p, mname}
		if v, ok := perModel[k]; ok {
			return v
		}
		v := &ModelBreakdown{Provider: p, Model: mname}
		perModel[k] = v
		return v
	}

	for _, f := range families {
		switch f.GetName() {
		case "gateway_requests_total":
			for _, m := range f.GetMetric() {
				v := uint64(m.GetCounter().GetValue())
				summary.RequestsTotal += v
				switch statusLabelFromMetric(m) {
				case "401", "403", "404", "413", "429", "4xx":
					summary.Errors4xx += v
				case "502", "503", "504", "5xx":
					summary.Errors5xx += v
				}
				if p, mname := labelValue(m, "provider"), labelValue(m, "model"); p != "" || mname != "" {
					getOrInit(p, mname).Requests += v
				}
			}
		case "gateway_cost_micro_total":
			for _, m := range f.GetMetric() {
				cost := int64(m.GetCounter().GetValue())
				summary.CostMicroTotal += cost
				if p, mname := labelValue(m, "provider"), labelValue(m, "model"); p != "" || mname != "" {
					getOrInit(p, mname).CostMicro += cost
				}
			}
		case "gateway_tokens_total":
			for _, m := range f.GetMetric() {
				v := uint64(m.GetCounter().GetValue())
				p, mname := labelValue(m, "provider"), labelValue(m, "model")
				if p == "" && mname == "" {
					continue
				}
				row := getOrInit(p, mname)
				switch labelValue(m, "type") {
				case "prompt":
					row.PromptTokens += v
				case "completion":
					row.CompletionTokens += v
				}
			}
		case "gateway_request_duration_seconds":
			// p99 is the histogram quantile across ALL labels (we
			// don't split by provider/model here — the dashboard
			// shows one number). The Histogram aggregation merges
			// buckets by adding cumulative counts.
			merged := mergeHistograms(f.GetMetric())
			summary.P99LatencySeconds = histogramQuantile(0.99, merged)
		}
	}
	summary.TopModels = topModels(perModel)

	s.mu.Lock()
	cp := summary
	s.cached = &cp
	s.cachedAt = now
	s.mu.Unlock()
	return &summary, nil
}

// statusLabelFromMetric pulls the "status" label out of a labeled
// metric. Returns "" when missing — the caller's switch then drops
// the value into neither bucket, which is correct (we don't know
// what to count it as).
func statusLabelFromMetric(m *dto.Metric) string {
	return labelValue(m, "status")
}

// labelValue returns the value of `name` on m, or "" if absent.
// Generic accessor reused by cache_stats.go and elsewhere — beats
// per-label one-off helpers.
func labelValue(m *dto.Metric, name string) string {
	for _, lp := range m.GetLabel() {
		if lp.GetName() == name {
			return lp.GetValue()
		}
	}
	return ""
}

// mergedHistogram aggregates per-label histograms into one. Buckets
// align (Prometheus enforces this when the histogram is registered
// once) so we just add cumulative counts and the sum.
type mergedHistogram struct {
	sampleCount uint64
	sampleSum   float64
	buckets     []bucket
}

type bucket struct {
	upper float64
	count uint64
}

func mergeHistograms(metrics []*dto.Metric) mergedHistogram {
	if len(metrics) == 0 {
		return mergedHistogram{}
	}
	first := metrics[0].GetHistogram()
	if first == nil {
		return mergedHistogram{}
	}
	out := mergedHistogram{
		buckets: make([]bucket, len(first.GetBucket())),
	}
	for i, b := range first.GetBucket() {
		out.buckets[i] = bucket{upper: b.GetUpperBound()}
	}

	for _, m := range metrics {
		h := m.GetHistogram()
		if h == nil {
			continue
		}
		out.sampleCount += h.GetSampleCount()
		out.sampleSum += h.GetSampleSum()
		for i, b := range h.GetBucket() {
			if i >= len(out.buckets) {
				break
			}
			out.buckets[i].count += b.GetCumulativeCount()
		}
	}
	return out
}

// histogramQuantile estimates the qth quantile from cumulative
// bucket counts via linear interpolation within the matching bucket.
// Mirrors Prometheus's `histogram_quantile` semantics for the same
// reason: produce a defensible single number from coarse buckets.
//
// Returns 0 when there's no data so the WebUI displays "—" without
// special-case logic.
func histogramQuantile(q float64, h mergedHistogram) float64 {
	if h.sampleCount == 0 || len(h.buckets) == 0 {
		return 0
	}
	target := q * float64(h.sampleCount)

	var prevUpper float64
	var prevCount uint64
	for _, b := range h.buckets {
		if float64(b.count) >= target {
			// Interpolate within this bucket. Width is upper - prev;
			// fraction is (target - prevCount) / (count - prevCount).
			width := b.upper - prevUpper
			countInBucket := float64(b.count - prevCount)
			if countInBucket <= 0 || width <= 0 {
				return b.upper
			}
			frac := (target - float64(prevCount)) / countInBucket
			return prevUpper + frac*width
		}
		prevUpper = b.upper
		prevCount = b.count
	}
	// Above all buckets — return the highest upper bound (+Inf is
	// usually the last bucket, in which case 0 falls out implicitly).
	return h.buckets[len(h.buckets)-1].upper
}

// topModels returns the breakdown rows sorted by request count
// descending (cost is the tie-breaker). Capped at topModelsLimit so
// the WebUI table stays readable; for higher cardinality go look
// at Grafana.
func topModels(perModel map[modelKey]*ModelBreakdown) []ModelBreakdown {
	if len(perModel) == 0 {
		return nil
	}
	out := make([]ModelBreakdown, 0, len(perModel))
	for _, v := range perModel {
		out = append(out, *v)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Requests != out[j].Requests {
			return out[i].Requests > out[j].Requests
		}
		return out[i].CostMicro > out[j].CostMicro
	})
	if len(out) > topModelsLimit {
		out = out[:topModelsLimit]
	}
	return out
}
