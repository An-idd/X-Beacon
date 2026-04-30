package observability

import (
	"time"

	"github.com/prometheus/client_golang/prometheus"
)

// Metrics is the bundle of X-BEACON-specific Prometheus metrics. One
// instance is constructed at startup and threaded into the chat handler,
// router, ratelimit middleware, billing worker, etc. via server.Deps.
//
// Each method on Metrics is goroutine-safe and lock-free (Prometheus
// metric types are designed for high-frequency hot-path access).
//
// Naming convention: every metric is prefixed `gateway_` so a Grafana
// dashboard or PromQL alert can scope to this service without touching
// scrape labels. Histograms use SI units (seconds) per Prometheus
// best-practice; cost is tracked in BIGINT micro-units to match the
// internal billing arithmetic and avoid float drift.
type Metrics struct {
	// CLAUDE.md "必须有的核心指标" — the six mandatory gauges/counters.

	// requestsTotal counts HTTP responses by upstream provider, model,
	// and final status. Status is the gateway's external status code,
	// not the upstream's (which may have been remapped).
	requestsTotal *prometheus.CounterVec

	// requestDuration is wall-clock latency from the chat handler's
	// start (after parse) to response written. Excludes auth /
	// ratelimit / decode time so this number tracks "what the user's
	// chosen upstream cost".
	requestDuration *prometheus.HistogramVec

	// tokensTotal counts tokens by provider/model/type. type ∈
	// {"prompt", "completion"}. Source preference: upstream Usage
	// first, tokenizer fallback if absent (pkg/tokenizer).
	tokensTotal *prometheus.CounterVec

	// costMicroTotal accumulates micro-units of currency. Grafana
	// displays as float (divide by 1_000_000). Labeled by provider /
	// model / api_key_id so per-tenant rollups need no joins.
	costMicroTotal *prometheus.CounterVec

	// cacheHitsTotal — exact / semantic (Week 9 fills these in;
	// declared now so dashboards don't break later).
	cacheHitsTotal *prometheus.CounterVec

	// cacheWritesTotal counts successful Set() calls per cache type.
	// Comparing this against cacheHitsTotal of the same type gives the
	// rough hit/write ratio (a higher write count than hit count means
	// the cache is filling up but not being read — a tuning signal).
	cacheWritesTotal *prometheus.CounterVec

	// cacheLookupDuration measures backend latency by outcome
	// (hit|miss|error). Sub-millisecond expected; bucketing skews low
	// because anything over 25 ms means Redis is the bottleneck.
	cacheLookupDuration *prometheus.HistogramVec

	// semanticSimilarity records the cosine similarity (0..1) of the
	// best neighbor returned by the semantic cache. Observed on both
	// hits and below-threshold misses so the threshold can be tuned
	// from observed near-miss distributions.
	semanticSimilarity prometheus.Histogram

	// semanticThreshold mirrors the configured cosine-similarity
	// threshold so dashboards can overlay "where the line is" against
	// the similarity histogram. Set once at startup; updated via
	// SetSemanticThreshold if a future feature reloads it.
	semanticThreshold prometheus.Gauge

	// semanticLookupDuration covers the embed + KNN round trip.
	// Buckets skew higher than the exact-cache version because embed
	// alone is ~50-200ms; the histogram makes "embed slow" vs "KNN
	// slow" diagnosable when paired with cacheLookupDuration.
	semanticLookupDuration *prometheus.HistogramVec

	// routerDecisionTotal counts the requests that were actually
	// rerouted by smart routing (Week 11). NOT a per-request counter —
	// the "no rule matched" path is intentionally not recorded here.
	// Pair with gateway_requests_total to compute reroute share.
	routerDecisionTotal *prometheus.CounterVec

	// routerBypassTotal counts requests where the classifier was
	// SKIPPED for an explicit reason (e.g. smart_route:disable scope).
	// Separate from decision_total so an A/B opt-out alert can target
	// just this metric: a sudden drop to 0 means the scope check broke.
	routerBypassTotal *prometheus.CounterVec

	// ratelimitRejectedTotal counts 429s by rule name (the user's
	// configured `name` in rate_limits[]).
	ratelimitRejectedTotal *prometheus.CounterVec

	// Week 6 additions — failover and breaker observability.

	// routerFailoverTotal counts hops in the chain loop. `from` and
	// `to` are the provider names; one row per hop, so a single
	// request that hops twice records two events.
	routerFailoverTotal *prometheus.CounterVec

	// breakerState reports the current circuit breaker state per
	// provider (0=closed, 1=half-open, 2=open). Updated on every
	// state transition by the router.
	breakerState *prometheus.GaugeVec

	// Week 7 additions — billing pipeline health.

	// billingDroppedTotal increments on Worker.Enqueue rejections
	// (channel buffer was full). Spike here = downstream DB slow.
	billingDroppedTotal prometheus.Counter

	// billingWrittenTotal increments after successful request_logs
	// INSERT. Comparison with requestsTotal gives the "did we record
	// every billable request" answer.
	billingWrittenTotal prometheus.Counter

	// pricingCacheSize is the number of model_pricing rows currently
	// loaded. Drops to 0 = startup reload failed; grows after admin
	// upserts or the periodic reload.
	pricingCacheSize prometheus.Gauge

	// Week 12 additions — prompt compression observability.

	// promptCompressedTotal counts the requests that actually had
	// messages dropped. Below-trigger requests are NOT recorded here
	// (would dominate the counter and hide the signal). Pair with
	// gateway_requests_total for compression share per model.
	promptCompressedTotal *prometheus.CounterVec

	// promptTokensSaved is a histogram of (TokensBefore - TokensAfter)
	// — i.e. how much was actually trimmed. Observed only when
	// compression fired so the histogram isn't pinned at 0.
	promptTokensSaved prometheus.Histogram

	// timeseries is the in-memory ring buffer feeding
	// /admin/stats/timeseries. Updated alongside Prometheus counters
	// in ObserveRequest. Reset on process restart by design (see
	// TimeSeries doc); /admin/stats/summary publishes `since` for
	// dashboard transparency.
	timeseries *TimeSeries

	// startedAt is the construction time of this Metrics bundle —
	// used by /admin/stats/summary to publish a `since` field so
	// the WebUI can show "data since <when>". Same lifetime as
	// timeseries; promoted to its own field because tests may want
	// to pin it independently.
	startedAt time.Time
}

// latencyBuckets covers the LLM-gateway dynamic range: ~5 ms (cache
// hit) through ~30 s (long completion). Narrower than Prometheus
// defaults at the low end so P99 < 10 ms gets a useful resolution.
var latencyBuckets = []float64{
	0.005, 0.01, 0.025, 0.05, 0.1,
	0.25, 0.5, 1, 2.5, 5, 10, 30,
}

// cacheLookupBuckets covers Redis-backed cache reads: a healthy LAN
// hop is ~100-300 µs; anything past 10 ms is a failure mode worth
// alerting on. Tighter than latencyBuckets at the low end because the
// signal we care about is sub-millisecond.
var cacheLookupBuckets = []float64{
	0.0001, 0.00025, 0.0005, 0.001, 0.0025,
	0.005, 0.01, 0.025, 0.1,
}

// semanticLookupBuckets covers embed + KNN. Embed alone is ~50-200ms
// at the cheapest OpenAI model; KNN ~1-5ms in steady-state RediSearch.
// The high bound (5s) catches embedder cold-starts and Redis swapping.
var semanticLookupBuckets = []float64{
	0.005, 0.01, 0.025, 0.05, 0.1,
	0.25, 0.5, 1, 2.5, 5,
}

// NewMetrics constructs and registers the gateway metrics on reg.
// Returns the bundle plus an error from MustRegister surfaced as a
// regular error (so tests can fail cleanly instead of panicking).
func NewMetrics(reg prometheus.Registerer) (*Metrics, error) {
	m := &Metrics{
		requestsTotal: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "gateway_requests_total",
			Help: "Total /v1/chat/completions responses by provider, model, and status.",
		}, []string{"provider", "model", "status"}),

		requestDuration: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Name:    "gateway_request_duration_seconds",
			Help:    "Wall-clock latency of /v1/chat/completions from handler entry to response written.",
			Buckets: latencyBuckets,
		}, []string{"provider", "model"}),

		tokensTotal: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "gateway_tokens_total",
			Help: "Total tokens accounted by provider, model, and type (prompt|completion).",
		}, []string{"provider", "model", "type"}),

		costMicroTotal: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "gateway_cost_micro_total",
			Help: "Total cost in micro-units of currency (1 USD = 1_000_000 micro). Grafana divides for display.",
		}, []string{"provider", "model", "api_key_id"}),

		cacheHitsTotal: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "gateway_cache_hits_total",
			Help: "Cache hit count by type (exact|semantic). Populated in Week 9; declared now for dashboard stability.",
		}, []string{"type"}),

		cacheWritesTotal: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "gateway_cache_writes_total",
			Help: "Successful cache Set() count by type (exact|semantic). Excludes responses filtered by the anti-pollution gate.",
		}, []string{"type"}),

		cacheLookupDuration: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Name:    "gateway_cache_lookup_duration_seconds",
			Help:    "Cache Get() backend latency by result (hit|miss|error).",
			Buckets: cacheLookupBuckets,
		}, []string{"result"}),

		semanticSimilarity: prometheus.NewHistogram(prometheus.HistogramOpts{
			Name:    "gateway_cache_semantic_similarity",
			Help:    "Cosine similarity (0..1) of the best neighbor on each semantic lookup; observed on both hits and below-threshold misses.",
			Buckets: prometheus.LinearBuckets(0.5, 0.05, 11),
		}),

		semanticThreshold: prometheus.NewGauge(prometheus.GaugeOpts{
			Name: "gateway_cache_semantic_threshold",
			Help: "Configured cosine-similarity threshold for semantic cache hits (0..1). Overlay against gateway_cache_semantic_similarity to see near-miss distribution vs. the cutoff.",
		}),

		semanticLookupDuration: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Name:    "gateway_cache_semantic_lookup_duration_seconds",
			Help:    "End-to-end latency of one semantic cache lookup (embed + KNN + threshold gate) by result (hit|miss|error).",
			Buckets: semanticLookupBuckets,
		}, []string{"result"}),

		routerDecisionTotal: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "gateway_router_decision_total",
			Help: "Smart-routing decisions that actually rewrote req.Model. Pair with gateway_requests_total for rerouting share.",
		}, []string{"from", "to", "rule"}),

		routerBypassTotal: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "gateway_router_bypass_total",
			Help: "Smart-routing skipped explicitly. reason=scope when a smart_route:disable principal opted out.",
		}, []string{"reason"}),

		ratelimitRejectedTotal: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "gateway_ratelimit_rejected_total",
			Help: "429s issued, labeled by the rate-limit rule that triggered.",
		}, []string{"rule"}),

		routerFailoverTotal: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "gateway_router_failover_total",
			Help: "Failover hops in the chain loop, labeled by source/target provider.",
		}, []string{"from", "to"}),

		breakerState: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: "gateway_breaker_state",
			Help: "Per-provider circuit breaker state: 0=closed, 1=half-open, 2=open.",
		}, []string{"provider"}),

		billingDroppedTotal: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "gateway_billing_dropped_total",
			Help: "Billing events dropped because the worker's buffer was full.",
		}),

		billingWrittenTotal: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "gateway_billing_written_total",
			Help: "Billing events successfully INSERTed into request_logs.",
		}),

		pricingCacheSize: prometheus.NewGauge(prometheus.GaugeOpts{
			Name: "gateway_pricing_cache_size",
			Help: "Number of model_pricing rows currently loaded into the in-memory cache.",
		}),

		promptCompressedTotal: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "gateway_prompt_compressed_total",
			Help: "Requests where the prompt compressor dropped at least one message, labeled by model.",
		}, []string{"model"}),

		promptTokensSaved: prometheus.NewHistogram(prometheus.HistogramOpts{
			Name:    "gateway_prompt_tokens_saved",
			Help:    "Tokens removed from the prompt by the compressor. Observed only when compression fired.",
			Buckets: prometheus.ExponentialBuckets(64, 2, 12), // 64 → 131072
		}),

		timeseries: NewTimeSeries(),
		startedAt:  time.Now(),
	}

	collectors := []prometheus.Collector{
		m.requestsTotal, m.requestDuration, m.tokensTotal,
		m.costMicroTotal, m.cacheHitsTotal, m.cacheWritesTotal,
		m.cacheLookupDuration, m.semanticSimilarity, m.semanticThreshold,
		m.semanticLookupDuration, m.routerDecisionTotal, m.routerBypassTotal,
		m.ratelimitRejectedTotal,
		m.routerFailoverTotal, m.breakerState,
		m.billingDroppedTotal, m.billingWrittenTotal, m.pricingCacheSize,
		m.promptCompressedTotal, m.promptTokensSaved,
	}
	for _, c := range collectors {
		if err := reg.Register(c); err != nil {
			return nil, err
		}
	}
	return m, nil
}

// --- Hot-path helpers. Each is goroutine-safe and lock-free. ---

// ObserveRequest records the response of one /v1/chat/completions call.
// status is the gateway's external HTTP status, not the upstream's.
//
// Also feeds the in-memory TimeSeries used by /admin/stats/timeseries.
// Single integration point keeps the chat handler's call sites
// branch-free — every ObserveRequest is also a timeseries record.
func (m *Metrics) ObserveRequest(provider, model string, status int, durationSec float64) {
	if m == nil {
		return
	}
	m.requestsTotal.WithLabelValues(provider, model, statusLabel(status)).Inc()
	m.requestDuration.WithLabelValues(provider, model).Observe(durationSec)
	m.timeseries.Record(status)
}

// TimeSeries returns the embedded /admin/stats/timeseries source.
// nil-safe so callers in dev mode (no metrics) don't have to guard.
func (m *Metrics) TimeSeries() *TimeSeries {
	if m == nil {
		return nil
	}
	return m.timeseries
}

// StartedAt returns when this Metrics bundle was constructed; used
// by /admin/stats/summary to publish a `since` field.
func (m *Metrics) StartedAt() time.Time {
	if m == nil {
		return time.Time{}
	}
	return m.startedAt
}

// AddTokens records prompt + completion token counts. Both halves are
// reported even when one is zero so PromQL `sum by (type)` works.
func (m *Metrics) AddTokens(provider, model string, prompt, completion int) {
	if m == nil {
		return
	}
	if prompt > 0 {
		m.tokensTotal.WithLabelValues(provider, model, "prompt").Add(float64(prompt))
	}
	if completion > 0 {
		m.tokensTotal.WithLabelValues(provider, model, "completion").Add(float64(completion))
	}
}

// AddCost increments cost in micro-units (matches billing.Cost output).
// apiKeyID may be "" when auth is disabled (dev mode); empty label is
// preferred over missing-label (Prometheus drops series with all-empty).
func (m *Metrics) AddCost(provider, model, apiKeyID string, micro int64) {
	if m == nil || micro == 0 {
		return
	}
	m.costMicroTotal.WithLabelValues(provider, model, apiKeyID).Add(float64(micro))
}

// IncCacheHit bumps the exact|semantic counter (Week 9 wires this).
func (m *Metrics) IncCacheHit(kind string) {
	if m == nil {
		return
	}
	m.cacheHitsTotal.WithLabelValues(kind).Inc()
}

// IncCacheWrite increments after a successful cache Set(). Skipped
// writes (anti-pollution gate, write errors) are intentionally not
// counted — read this together with cacheHitsTotal to detect a cache
// that's filling but not being read.
func (m *Metrics) IncCacheWrite(kind string) {
	if m == nil {
		return
	}
	m.cacheWritesTotal.WithLabelValues(kind).Inc()
}

// ObserveCacheLookup records the latency of one Get() call. result
// must be one of "hit", "miss", "error" so PromQL can split them.
func (m *Metrics) ObserveCacheLookup(result string, durationSec float64) {
	if m == nil {
		return
	}
	m.cacheLookupDuration.WithLabelValues(result).Observe(durationSec)
}

// ObserveSemanticSimilarity records the cosine similarity of the best
// neighbor seen on one semantic lookup. Skipped silently when
// similarity == 0 (no candidates / empty index) — recording zeros
// would skew the threshold-tuning view.
func (m *Metrics) ObserveSemanticSimilarity(similarity float64) {
	if m == nil || similarity <= 0 {
		return
	}
	m.semanticSimilarity.Observe(similarity)
}

// SetSemanticThreshold publishes the configured similarity threshold
// (0..1) so dashboards can overlay it against the similarity
// histogram. Idempotent; called at startup and any time the
// threshold is reloaded.
func (m *Metrics) SetSemanticThreshold(threshold float64) {
	if m == nil {
		return
	}
	m.semanticThreshold.Set(threshold)
}

// ObserveSemanticLookup records the end-to-end latency of one
// semantic cache lookup. result must be one of "hit", "miss", "error"
// (matching the exact-cache lookup labels).
func (m *Metrics) ObserveSemanticLookup(result string, durationSec float64) {
	if m == nil {
		return
	}
	m.semanticLookupDuration.WithLabelValues(result).Observe(durationSec)
}

// IncRouterDecision counts one actual reroute. Caller passes the
// pre-route model id, the post-route model id, and the rule name
// that fired.
func (m *Metrics) IncRouterDecision(from, to, rule string) {
	if m == nil {
		return
	}
	m.routerDecisionTotal.WithLabelValues(from, to, rule).Inc()
}

// IncRouterBypass counts one classifier-skipped request. reason should
// be a stable token: currently only "scope".
func (m *Metrics) IncRouterBypass(reason string) {
	if m == nil {
		return
	}
	m.routerBypassTotal.WithLabelValues(reason).Inc()
}

// IncRatelimitReject bumps the rate-limit rejection counter for rule.
func (m *Metrics) IncRatelimitReject(rule string) {
	if m == nil {
		return
	}
	m.ratelimitRejectedTotal.WithLabelValues(rule).Inc()
}

// IncFailover counts one failover hop from src→dst provider.
func (m *Metrics) IncFailover(from, to string) {
	if m == nil {
		return
	}
	m.routerFailoverTotal.WithLabelValues(from, to).Inc()
}

// SetBreakerState updates the provider's circuit breaker state gauge.
// Pass 0/1/2 for closed/half-open/open.
func (m *Metrics) SetBreakerState(provider string, state int) {
	if m == nil {
		return
	}
	m.breakerState.WithLabelValues(provider).Set(float64(state))
}

// IncBillingDropped is called on Worker.Enqueue → channel-full drop.
func (m *Metrics) IncBillingDropped() {
	if m == nil {
		return
	}
	m.billingDroppedTotal.Inc()
}

// IncBillingWritten is called on a successful request_logs INSERT.
func (m *Metrics) IncBillingWritten() {
	if m == nil {
		return
	}
	m.billingWrittenTotal.Inc()
}

// SetPricingCacheSize is called after every PricingCache reload / Set
// / Delete. Constant-time write, fine for the hot-ish admin path.
func (m *Metrics) SetPricingCacheSize(n int) {
	if m == nil {
		return
	}
	m.pricingCacheSize.Set(float64(n))
}

// ObservePromptCompressed records one request that had messages
// dropped. tokensSaved is the difference between pre- and post-
// compression prompt tokens; values <= 0 are skipped (defensive —
// a successful compression always reduces tokens).
func (m *Metrics) ObservePromptCompressed(model string, tokensSaved int) {
	if m == nil {
		return
	}
	m.promptCompressedTotal.WithLabelValues(model).Inc()
	if tokensSaved > 0 {
		m.promptTokensSaved.Observe(float64(tokensSaved))
	}
}

// statusLabel coalesces ranges so we don't explode cardinality with
// every possible status code. Pattern lifted from Envoy / Istio.
func statusLabel(status int) string {
	switch {
	case status >= 200 && status < 300:
		return "2xx"
	case status >= 300 && status < 400:
		return "3xx"
	case status >= 400 && status < 500:
		// 4xx classes we care about individually for billing /
		// ratelimit dashboards.
		switch status {
		case 401:
			return "401"
		case 403:
			return "403"
		case 404:
			return "404"
		case 413:
			return "413"
		case 429:
			return "429"
		default:
			return "4xx"
		}
	case status >= 500 && status < 600:
		switch status {
		case 502:
			return "502"
		case 503:
			return "503"
		case 504:
			return "504"
		default:
			return "5xx"
		}
	default:
		return "unknown"
	}
}
