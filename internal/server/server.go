// Package server assembles the HTTP routing surface for the gateway.
//
// Step 3.1 establishes the boundary: cmd/gateway/main.go is responsible for
// loading config and constructing dependencies; this package consumes those
// dependencies and returns an http.Handler. Subsequent steps (3.2 middleware,
// 3.3 auth, 3.4-3.5 /v1/chat/completions) attach to this same Server.
package server

import (
	"net/http"

	"github.com/go-chi/chi/v5"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"go.uber.org/zap"

	"github.com/An-idd/x-beacon/internal/auth"
	"github.com/An-idd/x-beacon/internal/provider/registry"
	"github.com/An-idd/x-beacon/internal/ratelimit"
	"github.com/An-idd/x-beacon/internal/server/middleware"
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

	// ReadinessCheckers feed /readyz. Order is preserved in the JSON body
	// for stable parsing. nil/empty makes /readyz a trivial 200.
	ReadinessCheckers []ReadinessChecker

	// MetricsEnabled / MetricsPath are pulled out of config so server stays
	// agnostic to the full config shape — easier to test and reuse.
	MetricsEnabled bool
	MetricsPath    string

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
		v1.Use(middleware.RateLimit(deps.RateLimiter, deps.Logger))
		// OpenAI-compatible model catalog. Handler tolerates an empty registry
		// (returns {"object":"list","data":[]}) so the gateway boots even when
		// providers.yaml is absent.
		v1.Get("/models", modelsHandler(deps.Registry))
		v1.Post("/chat/completions", chatCompletionsHandler(deps.Registry, deps.Logger))
	})

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
