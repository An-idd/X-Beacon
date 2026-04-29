package observability

import (
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
	}

	collectors := []prometheus.Collector{
		m.requestsTotal, m.requestDuration, m.tokensTotal,
		m.costMicroTotal, m.cacheHitsTotal, m.cacheWritesTotal,
		m.cacheLookupDuration, m.ratelimitRejectedTotal,
		m.routerFailoverTotal, m.breakerState,
		m.billingDroppedTotal, m.billingWrittenTotal, m.pricingCacheSize,
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
func (m *Metrics) ObserveRequest(provider, model string, status int, durationSec float64) {
	if m == nil {
		return
	}
	m.requestsTotal.WithLabelValues(provider, model, statusLabel(status)).Inc()
	m.requestDuration.WithLabelValues(provider, model).Observe(durationSec)
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
