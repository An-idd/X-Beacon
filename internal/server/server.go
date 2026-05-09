// Package server assembles the HTTP routing surface for the gateway.
//
// Step 3.1 establishes the boundary: cmd/gateway/main.go is responsible for
// loading config and constructing dependencies; this package consumes those
// dependencies and returns an http.Handler. Subsequent steps (3.2 middleware,
// 3.3 auth, 3.4-3.5 /v1/chat/completions) attach to this same Server.
package server

import (
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"go.uber.org/zap"

	"github.com/An-idd/x-beacon/internal/auth"
	"github.com/An-idd/x-beacon/internal/billing"
	"github.com/An-idd/x-beacon/internal/cache"
	"github.com/An-idd/x-beacon/internal/observability"
	"github.com/An-idd/x-beacon/internal/prompt"
	"github.com/An-idd/x-beacon/internal/provider/registry"
	"github.com/An-idd/x-beacon/internal/ratelimit"
	"github.com/An-idd/x-beacon/internal/route"
	"github.com/An-idd/x-beacon/internal/router"
	"github.com/An-idd/x-beacon/internal/server/middleware"
	"github.com/An-idd/x-beacon/internal/storage"
	"github.com/An-idd/x-beacon/pkg/tokenizer"
)

// Deps groups everything the server needs from main. New dependencies added
// in later steps (auth, billing, ratelimit) extend this struct rather than
// adding parameters to New(), keeping the call site stable.
type Deps struct {
	Logger     *zap.Logger
	Registry   *registry.Registry
	MetricsReg *prometheus.Registry

	// Authn enforces bearer-token auth on /v1/* routes. May be nil during
	// early bootstrap (no auth.yaml + dev mode); the server then leaves
	// /v1/* unauthenticated and logs a warn line at startup.
	Authn auth.Authenticator

	// RateLimiter enforces configured rules on /v1/* routes (after Auth).
	// nil/empty Multi → no-op; rate-limit middleware short-circuits and
	// the chain runs without any per-request rate-limit cost.
	RateLimiter *ratelimit.Multi

	// Router orchestrates retry / fail-over / circuit-breaker decisions for
	// chat completions. main constructs it from Registry + RouterConfig.
	// Required when chat handlers are mounted (i.e. always in the current
	// route surface); nil triggers a missing-dep error in New.
	Router *router.Router

	// Tokenizer selects the right token-counting implementation per
	// model id (cl100k_base for OpenAI/DeepSeek, scaled approximation
	// for Anthropic). Optional — nil disables prompt-token attribution
	// in billing events; the worker will record zero token counts.
	Tokenizer *tokenizer.Selector

	// Billing accepts request events asynchronously. Optional — nil
	// disables billing entirely (events go nowhere); chat handlers
	// continue to serve traffic unaffected.
	Billing *billing.Worker

	// Pricing is the in-memory model→rate cache. When non-nil, the
	// /admin/pricing routes are mounted and protected by the
	// admin:pricing scope. Optional in dev mode.
	Pricing *billing.PricingCache

	// Keystore exposes /admin/keys CRUD. When non-nil, list / create /
	// revoke endpoints mount under /admin/keys with the admin:webui
	// scope guard. Optional in dev mode (no DB → no Keystore → no
	// admin keys surface; xbctl still works for ops out-of-band).
	Keystore *auth.Keystore

	// StoragePool backs /admin/logs (request_logs lookups). When
	// non-nil, the logs endpoint mounts under /admin/logs with the
	// admin:webui scope guard. Optional — dev mode without DB skips
	// this surface.
	StoragePool *storage.Pool

	// Stats backs /admin/stats/summary. When non-nil, the summary
	// endpoint mounts under the admin:webui scope guard. Built from
	// the same registry that serves /metrics; cached at the
	// collector layer.
	Stats *observability.StatsCollector

	// Metrics is the gateway-specific Prometheus metric bundle (Week
	// 8). Optional; nil-safe helpers everywhere so dev-mode without a
	// metrics scrape target still serves traffic.
	Metrics *observability.Metrics

	// Cache is the exact-match response cache (Week 9). Optional —
	// when nil, the chat handler skips the lookup/store path entirely.
	// Both streaming and non-streaming branches consult it from
	// Week 10; streaming hits replay as synthetic SSE.
	Cache cache.Exact

	// CacheTTL is how long a successfully-cached response lives. Read
	// from cache.exact.ttl in config.yaml; 0 disables writes (reads
	// already short-circuit on a nil Cache).
	CacheTTL time.Duration

	// Semantic is the similarity-based response cache (Week 10).
	// Optional — when nil, the chat handler skips the semantic
	// pipeline entirely and only consults Cache. When non-nil, the
	// chat handler queries Semantic on exact-miss and writes to it
	// alongside Cache on successful upstream responses.
	Semantic cache.Semantic

	// Compressor is the prompt-truncation layer (Week 12). Optional —
	// when nil, prompts pass through verbatim. When non-nil, the
	// chat handler runs Compress *after* Classifier (so routing
	// sees the original prompt) and *before* the cache lookup (so
	// cache keys reflect the truncated form). Mutates req.Messages
	// in place on a non-trivial Result.
	Compressor prompt.Compressor

	// Classifier is the smart-routing layer (Week 11). Optional —
	// when nil, requests pass through with their requested model
	// unchanged. When non-nil, the chat handler runs Classify before
	// cache + router, and mutates req.Model on a non-empty Decision
	// (Choice A: cache keys + billing reflect the routed model).
	Classifier route.Classifier

	// ReadinessCheckers feed /readyz. Order is preserved in the JSON body
	// for stable parsing. nil/empty makes /readyz a trivial 200.
	ReadinessCheckers []ReadinessChecker

	// MetricsEnabled / MetricsPath are pulled out of config so server stays
	// agnostic to the full config shape — easier to test and reuse.
	MetricsEnabled bool
	MetricsPath    string

	// AdminCORSOrigins is the explicit allowlist for /admin/* CORS.
	// Empty (default) = no CORS headers emitted; browsers reject any
	// cross-origin request. List each WebUI host explicitly; wildcards
	// are not supported by the middleware.
	AdminCORSOrigins []string

	// ServiceName labels OTel spans created by the Tracing middleware.
	// Defaults to "x-beacon" if empty.
	ServiceName string
}

// Server holds the assembled router. Constructed once at startup; safe for
// concurrent use under http.Server.
type Server struct {
	router chi.Router
	deps   Deps
}

// New validates Deps and constructs the router with all routes mounted.
// Middleware (Step 3.2) and auth (3.3) will attach inside this function in
// later steps; for now the route surface matches Week 1 (/healthz,
// /v1/models, /metrics).
func New(deps Deps) (*Server, error) {
	if deps.Logger == nil {
		return nil, errMissingDep("Logger")
	}
	if deps.Registry == nil {
		return nil, errMissingDep("Registry")
	}
	if deps.Router == nil {
		return nil, errMissingDep("Router")
	}
	if deps.MetricsEnabled && deps.MetricsReg == nil {
		return nil, errMissingDep("MetricsReg (required when MetricsEnabled)")
	}

	if deps.ServiceName == "" {
		deps.ServiceName = "x-beacon"
	}

	r := chi.NewRouter()

	// Middleware chain (outer → inner). Order matters: Recovery must be
	// outermost to catch panics in everything below; RequestID must precede
	// Tracing/Logging so they can include req_id; Logging is innermost to
	// observe final status/latency.
	skipPaths := []string{"/healthz"}
	if deps.MetricsEnabled {
		skipPaths = append(skipPaths, deps.MetricsPath)
	}
	r.Use(middleware.Recovery(deps.Logger))
	r.Use(middleware.RequestID())
	r.Use(middleware.Tracing(deps.ServiceName))
	r.Use(middleware.Logging(deps.Logger, middleware.LoggingOptions{SkipPaths: skipPaths}))

	// Liveness probe: process is up. Distinct from /readyz, which checks
	// downstream dependencies (DB, Redis) and refuses traffic when they
	// are unavailable.
	r.Get("/healthz", healthzHandler())
	r.Get("/readyz", readyzHandler(deps.ReadinessCheckers))

	// /v1/* lives on a subrouter so Auth can attach without leaking onto
	// /healthz or /metrics. Auth is mounted only when Authn is configured;
	// dev environments without auth.yaml still boot and serve /v1/models.
	r.Route("/v1", func(v1 chi.Router) {
		if deps.Authn != nil {
			v1.Use(middleware.Auth(deps.Authn, deps.Logger))
		} else {
			deps.Logger.Warn("auth disabled: /v1/* is unauthenticated",
				zap.String("hint", "configure auth_file in config.yaml to enable"))
		}
		// RateLimit runs AFTER Auth so per-key rules can pluck Principal.
		// nil/empty Multi short-circuits (no-op middleware).
		v1.Use(middleware.RateLimit(deps.RateLimiter, deps.Metrics, deps.Logger))
		// OpenAI-compatible model catalog. Handler tolerates an empty registry
		// (returns {"object":"list","data":[]}) so the gateway boots even when
		// providers.yaml is absent.
		v1.Get("/models", modelsHandler(deps.Registry))
		v1.Post("/chat/completions", chatCompletionsHandler(deps.Router, deps.Tokenizer, deps.Billing, deps.Metrics, deps.Cache, deps.CacheTTL, deps.Semantic, deps.Classifier, deps.Compressor, deps.Logger))
	})

	// /admin/* umbrella. CORS sits here, BEFORE Auth, so browser
	// preflight (OPTIONS, no Authorization) can complete the handshake.
	// Each sub-feature (pricing / keys / logs / stats) registers its
	// own subroute with its own scope guard inside the umbrella.
	//
	// Mounted only when Authn is configured — dev-mode without DB
	// boots and serves /v1/* but skips the admin surface entirely.
	if deps.Authn != nil {
		r.Route("/admin", func(adm chi.Router) {
			adm.Use(middleware.CORS(deps.AdminCORSOrigins))

			if deps.Pricing != nil {
				adm.Route("/pricing", func(p chi.Router) {
					p.Use(middleware.Auth(deps.Authn, deps.Logger))
					p.Use(middleware.RequireScope("admin", "pricing", deps.Logger))
					p.Mount("/", adminPricingHandlers(deps.Pricing, deps.Logger))
				})
			}

			if deps.Keystore != nil {
				adm.Route("/keys", func(k chi.Router) {
					k.Use(middleware.Auth(deps.Authn, deps.Logger))
					k.Use(middleware.RequireScope("admin", "webui", deps.Logger))
					k.Mount("/", adminKeysHandlers(deps.Keystore, deps.Logger))
				})
			}

			if deps.StoragePool != nil {
				adm.Route("/logs", func(l chi.Router) {
					l.Use(middleware.Auth(deps.Authn, deps.Logger))
					l.Use(middleware.RequireScope("admin", "webui", deps.Logger))
					l.Get("/", adminLogsHandler(deps.StoragePool, deps.Logger))
				})
			}

			if deps.Stats != nil && deps.Metrics != nil {
				adm.Route("/stats", func(s chi.Router) {
					s.Use(middleware.Auth(deps.Authn, deps.Logger))
					s.Use(middleware.RequireScope("admin", "webui", deps.Logger))
					s.Get("/summary", adminStatsSummaryHandler(deps.Stats, deps.Logger))
					s.Get("/timeseries", adminStatsTimeseriesHandler(deps.Metrics.TimeSeries()))
				})
			}

			// /admin/routing/rules: read-only view of active classifier
			// rules + per-rule hit counts. Mounted whenever auth is
			// configured — classifier may be nil (smart routing
			// disabled), in which case the handler reports
			// `enabled: false`.
			adm.Route("/routing", func(rt chi.Router) {
				rt.Use(middleware.Auth(deps.Authn, deps.Logger))
				rt.Use(middleware.RequireScope("admin", "webui", deps.Logger))
				rt.Get("/rules", adminRoutingRulesHandler(deps.Classifier, deps.MetricsReg, deps.Logger))
			})
		})
	}

	if deps.MetricsEnabled {
		r.Handle(deps.MetricsPath, promhttp.HandlerFor(
			deps.MetricsReg,
			promhttp.HandlerOpts{Registry: deps.MetricsReg},
		))
	}

	return &Server{router: r, deps: deps}, nil
}

// Handler returns the http.Handler suitable for http.Server.Handler.
func (s *Server) Handler() http.Handler { return s.router }

type missingDepError struct{ name string }

func (e *missingDepError) Error() string { return "server: missing dependency: " + e.name }

func errMissingDep(name string) error { return &missingDepError{name: name} }
