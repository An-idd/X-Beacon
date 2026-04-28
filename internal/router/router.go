// Package router sits between the HTTP handlers and the provider adapters.
// It owns retry, fail-over (Step 6.2), and circuit breaking (Step 6.3) — the
// concerns that span multiple provider calls. Provider adapters stay
// stateless; the router accumulates the cross-call decision state.
//
// Router resolves a model to a Provider via the registry (Week 1), then runs
// ChatCompletion under a RetryPolicy. Step 6.1 implements the retry loop
// against a single provider; Step 6.2 extends ResolveChain to multi-provider
// fail-over; Step 6.3 wraps each call site with a circuit breaker.
package router

import (
	"context"
	"errors"
	"fmt"
	"math/rand/v2"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"
	"go.uber.org/zap"

	"github.com/An-idd/x-beacon/internal/observability"
	"github.com/An-idd/x-beacon/internal/provider"
	"github.com/An-idd/x-beacon/internal/provider/registry"
)

// ModelResolver is the small slice of *registry.Registry that the router
// actually depends on. Defining it here (consumer-side) lets tests inject a
// stub without touching the registry's public surface.
//
// ResolveChain returns providers in priority order (primary first, then
// fail-over candidates). Empty slice means no provider can serve the model.
type ModelResolver interface {
	ResolveModel(model string) (provider.Provider, error)
	ResolveChain(model string) []provider.Provider
}

// Compile-time assertion: a real *registry.Registry must satisfy the
// resolver contract. If registry's signature ever drifts, this fails at
// build time rather than at server-wiring time.
var _ ModelResolver = (*registry.Registry)(nil)

// Router executes ChatCompletion (and later ChatCompletionStream) with
// retry / fail-over / circuit-breaker semantics on top of a ModelResolver.
type Router struct {
	resolver ModelResolver
	policy   RetryPolicy
	breakers *breakerPool
	metrics  *observability.Metrics // nil-safe; helpers no-op on nil
	logger   *zap.Logger

	// breakerSettings is staged during option application; the actual
	// breakerPool is built once at the end of New() so it observes the
	// final metrics handle injected via WithMetrics.
	breakerSettings BreakerSettings

	// Injectable for deterministic tests. nil → real time.Now / time.After /
	// math/rand/v2 uniform float. Tests that need to assert on backoff timing
	// supply controlled implementations.
	now    func() time.Time
	sleep  func(ctx context.Context, d time.Duration) error
	random func() float64
}

// Option configures a Router at construction time.
type Option func(*Router)

// WithClock injects a now() function. Used by tests to freeze time.
func WithClock(now func() time.Time) Option {
	return func(r *Router) { r.now = now }
}

// WithSleep injects a sleep function. Tests that don't want real wall-clock
// delays supply a no-op or a record-and-advance variant.
func WithSleep(sleep func(ctx context.Context, d time.Duration) error) Option {
	return func(r *Router) { r.sleep = sleep }
}

// WithRandom injects a [0,1) sampler. Tests that assert on the jitter
// envelope use a fixed value.
func WithRandom(random func() float64) Option {
	return func(r *Router) { r.random = random }
}

// WithBreakerSettings overrides the default circuit-breaker configuration.
// Mainly useful for tests that need fast-tripping breakers (low MinRequests,
// short Timeout) — production callers should accept DefaultBreakerSettings.
func WithBreakerSettings(s BreakerSettings) Option {
	return func(r *Router) { r.breakerSettings = s }
}

// WithMetrics injects the gateway metrics bundle so the router can emit
// failover and circuit-breaker observations. nil-safe.
func WithMetrics(m *observability.Metrics) Option {
	return func(r *Router) { r.metrics = m }
}

// New constructs a Router. policy is taken by value so callers can pass
// DefaultPolicy() and mutate locally without aliasing. resolver is typically
// a *registry.Registry from main; tests pass a stub.
func New(resolver ModelResolver, policy RetryPolicy, logger *zap.Logger, opts ...Option) *Router {
	r := &Router{
		resolver:        resolver,
		policy:          policy,
		logger:          logger,
		breakerSettings: DefaultBreakerSettings(),
		now:             time.Now,
		sleep:           defaultSleep,
		random:          rand.Float64,
	}
	for _, opt := range opts {
		opt(r)
	}
	// breakerPool is built last so OnStateChange closes over the final
	// metrics handle (WithMetrics may have arrived after WithBreakerSettings).
	r.breakers = newBreakerPool(r.breakerSettings, logger, r.metrics)
	return r
}

// ResolveModel exposes resolution to the server layer so handlers can
// produce 400 model_not_found before deciding to call the router. Keeping
// resolution here also lets Step 6.2 swap in fail-over chain resolution
// behind the same call.
func (r *Router) ResolveModel(model string) (provider.Provider, error) {
	return r.resolver.ResolveModel(model)
}

// ChatCompletion routes a non-streaming request through the configured
// retry-and-fail-over policy. Returns the final successful response or the
// last error.
//
// Two nested loops:
//   - Outer (chain): each provider in registry.ResolveChain priority order.
//     On retryable error, the next provider is tried with a fresh retry
//     budget (MaxRetries) but a SHARED time budget (MaxTotal anchored at
//     the first call).
//   - Inner (retry): full-jitter exponential within a single provider until
//     MaxRetries / MaxTotal exhausts or a non-retryable error appears.
//
// Non-retryable errors short-circuit: they propagate without trying the
// next provider in the chain. Failover only happens for transient
// conditions; request-shape problems (4xx) won't get healthier on a
// different upstream.
func (r *Router) ChatCompletion(ctx context.Context, req *provider.ChatRequest) (*provider.ChatResponse, error) {
	chain := r.resolver.ResolveChain(req.Model)
	if len(chain) == 0 {
		return nil, fmt.Errorf("%w: %q", registry.ErrNoProviderForModel, req.Model)
	}

	start := r.now()
	var lastErr error
	for chainIdx, p := range chain {
		resp, err := r.tryProvider(ctx, p, req, start)
		if err == nil {
			return resp, nil
		}
		lastErr = err

		// Breaker errors are synthesized at the start of tryProvider
		// (circuit was already open) — always failover-eligible.
		if !isBreakerError(err) && !provider.IsRetryable(err) {
			// Request-shape / auth / context-length: a different provider
			// won't help. Surface the original error.
			return nil, err
		}
		if r.policy.MaxTotal > 0 && r.now().Sub(start) >= r.policy.MaxTotal {
			r.logger.Warn("failover chain abandoned (time budget)",
				zap.String("model", req.Model),
				zap.Int("providers_tried", chainIdx+1),
				zap.Int("providers_remaining", len(chain)-chainIdx-1))
			break
		}
		if chainIdx+1 < len(chain) {
			r.logger.Info("failing over",
				zap.String("model", req.Model),
				zap.String("from", p.Name()),
				zap.String("to", chain[chainIdx+1].Name()),
				zap.Error(err))
			r.metrics.IncFailover(p.Name(), chain[chainIdx+1].Name())
		}
	}
	return nil, lastErr
}

// tryProvider runs the per-provider retry loop. start is the chain-wide
// anchor for MaxTotal so the time budget is not reset on failover.
//
// Each attempt passes through the provider's circuit breaker:
//   - Breaker open  → returns ErrOpenState immediately, no provider call,
//     no retry (treated as failover-eligible by the chain loop).
//   - Breaker closed/half-open → call proceeds; the breaker observes
//     success/failure and may trip after enough failures.
func (r *Router) tryProvider(ctx context.Context, p provider.Provider, req *provider.ChatRequest, start time.Time) (*provider.ChatResponse, error) {
	var lastErr error
	for attempt := 0; ; attempt++ {
		attemptCtx, attemptSpan := observability.Tracer().Start(ctx, "router.attempt",
			trace.WithAttributes(
				attribute.String("provider", p.Name()),
				attribute.String("model", req.Model),
				attribute.Int("attempt", attempt),
			),
		)
		done, gateErr := r.breakers.allow(p.Name())
		if gateErr != nil {
			attemptSpan.SetAttributes(attribute.String("breaker.state", "open"))
			attemptSpan.SetStatus(codes.Error, gateErr.Error())
			attemptSpan.End()
			r.logger.Warn("circuit breaker rejected request",
				zap.String("provider", p.Name()),
				zap.String("model", req.Model),
				zap.Error(gateErr))
			return nil, gateErr
		}
		callCtx, callSpan := observability.Tracer().Start(attemptCtx, "provider.call",
			trace.WithAttributes(attribute.String("provider", p.Name())),
		)
		resp, callErr := p.ChatCompletion(callCtx, req)
		if callErr != nil {
			callSpan.SetStatus(codes.Error, callErr.Error())
		}
		callSpan.End()
		// Only retryable / unavailable errors count against the breaker.
		// 4xx (request-shape, auth) shouldn't trip a healthy upstream.
		done(breakerObservation(callErr))
		if callErr == nil {
			attemptSpan.End()
			return resp, nil
		}
		attemptSpan.SetStatus(codes.Error, callErr.Error())
		attemptSpan.End()
		lastErr = callErr

		if !provider.IsRetryable(callErr) {
			return nil, callErr
		}
		if attempt >= r.policy.MaxRetries {
			break
		}

		retryAfter := extractRetryAfter(callErr)
		delay := r.policy.backoffDuration(attempt+1, retryAfter, r.random())

		if r.policy.MaxTotal > 0 {
			elapsed := r.now().Sub(start)
			if elapsed+delay > r.policy.MaxTotal {
				r.logger.Warn("retry budget exhausted (time)",
					zap.String("provider", p.Name()),
					zap.String("model", req.Model),
					zap.Int("attempt", attempt+1),
					zap.Duration("elapsed", elapsed),
					zap.Duration("would_sleep", delay))
				break
			}
		}

		r.logger.Debug("retrying chat completion",
			zap.String("provider", p.Name()),
			zap.String("model", req.Model),
			zap.Int("attempt", attempt+1),
			zap.Duration("delay", delay),
			zap.Error(callErr))

		if err := r.sleep(ctx, delay); err != nil {
			return nil, err
		}
	}
	return nil, lastErr
}

// ChatCompletionStream routes a streaming request through retry+failover
// with a critical boundary: retries are only attempted before the first
// chunk has been emitted. Once a StreamEvent{Chunk} has crossed back to
// the caller, the stream is "committed" — silently switching providers
// mid-stream would splice "first half OpenAI / second half Anthropic"
// into a single SSE response and break SDK parsers.
//
// Three error boundaries:
//
//	pre-call    — provider.ChatCompletionStream returns (nil, err): identical
//	              to non-streaming retry semantics; failover is fine.
//	pre-chunk   — channel's first event is StreamEvent{Err}: still retryable;
//	              client hasn't seen anything yet.
//	mid-stream  — channel emits StreamEvent{Err} after at least one chunk:
//	              forwarded to the caller as-is. NO retry, NO failover.
func (r *Router) ChatCompletionStream(ctx context.Context, req *provider.ChatRequest) (<-chan provider.StreamEvent, error) {
	chain := r.resolver.ResolveChain(req.Model)
	if len(chain) == 0 {
		return nil, fmt.Errorf("%w: %q", registry.ErrNoProviderForModel, req.Model)
	}

	start := r.now()
	var lastErr error
	for chainIdx, p := range chain {
		out, err := r.tryProviderStream(ctx, p, req, start)
		if err == nil {
			return out, nil
		}
		lastErr = err
		if !isBreakerError(err) && !provider.IsRetryable(err) {
			return nil, err
		}
		if r.policy.MaxTotal > 0 && r.now().Sub(start) >= r.policy.MaxTotal {
			r.logger.Warn("stream failover chain abandoned (time budget)",
				zap.String("model", req.Model),
				zap.Int("providers_tried", chainIdx+1))
			break
		}
		if chainIdx+1 < len(chain) {
			r.logger.Info("failing over (stream)",
				zap.String("model", req.Model),
				zap.String("from", p.Name()),
				zap.String("to", chain[chainIdx+1].Name()),
				zap.Error(err))
			r.metrics.IncFailover(p.Name(), chain[chainIdx+1].Name())
		}
	}
	return nil, lastErr
}

// tryProviderStream runs the per-provider retry loop for streaming. On
// success returns a forwarded channel that the handler writes through to
// the client; the underlying provider channel is drained by an internal
// goroutine that also notifies the breaker of mid-stream observations.
func (r *Router) tryProviderStream(ctx context.Context, p provider.Provider, req *provider.ChatRequest, start time.Time) (<-chan provider.StreamEvent, error) {
	var lastErr error
	for attempt := 0; ; attempt++ {
		out, err := r.streamAttemptOnce(ctx, p, req)
		if err == nil {
			return out, nil
		}
		lastErr = err

		if isBreakerError(err) {
			// Open circuit: don't retry within this provider; let the
			// chain loop decide whether to fail over.
			return nil, err
		}
		if !provider.IsRetryable(err) {
			return nil, err
		}
		if attempt >= r.policy.MaxRetries {
			break
		}

		retryAfter := extractRetryAfter(err)
		delay := r.policy.backoffDuration(attempt+1, retryAfter, r.random())

		if r.policy.MaxTotal > 0 {
			elapsed := r.now().Sub(start)
			if elapsed+delay > r.policy.MaxTotal {
				r.logger.Warn("stream retry budget exhausted (time)",
					zap.String("provider", p.Name()),
					zap.String("model", req.Model),
					zap.Int("attempt", attempt+1))
				break
			}
		}

		if sleepErr := r.sleep(ctx, delay); sleepErr != nil {
			return nil, sleepErr
		}
	}
	return nil, lastErr
}

// streamAttemptOnce runs one stream attempt:
//   - Reserve a breaker permit via Allow().
//   - Call provider.ChatCompletionStream.
//   - Peek the first event:
//   - first == Err: return err (caller may retry/failover).
//   - first == Chunk: spawn forwarder, return its out channel.
//
// On success, breaker.done is invoked by the forwarder when the stream
// terminates. On failure (pre-chunk), breaker.done is invoked here.
func (r *Router) streamAttemptOnce(ctx context.Context, p provider.Provider, req *provider.ChatRequest) (<-chan provider.StreamEvent, error) {
	done, gateErr := r.breakers.allow(p.Name())
	if gateErr != nil {
		r.logger.Warn("circuit breaker rejected stream",
			zap.String("provider", p.Name()),
			zap.String("model", req.Model),
			zap.Error(gateErr))
		return nil, gateErr
	}

	ch, callErr := p.ChatCompletionStream(ctx, req)
	if callErr != nil {
		done(breakerObservation(callErr))
		return nil, callErr
	}

	// Peek the first event. ctx cancellation here is treated as caller-gone:
	// release the breaker permit as a clean observation and return ctx.Err().
	var first provider.StreamEvent
	var ok bool
	select {
	case first, ok = <-ch:
	case <-ctx.Done():
		done(nil)
		return nil, ctx.Err()
	}
	if !ok {
		// Channel closed without emitting anything — provider violated its
		// contract or upstream cut us off before the first frame.
		ue := provider.NewUpstreamError(p.Name(), provider.ErrUpstream, 0, "stream closed before first event")
		done(breakerObservation(ue))
		return nil, ue
	}
	if first.Err != nil {
		done(breakerObservation(first.Err))
		return nil, first.Err
	}

	// Got a chunk — commit. Spawn forwarder so this function can return
	// quickly; the handler ranges the returned channel just like a direct
	// provider stream.
	out := make(chan provider.StreamEvent, 16)
	go r.forwardStream(ctx, ch, first, out, done)
	return out, nil
}

// forwardStream copies events from the provider's channel to out, starting
// with the already-peeked first chunk. Closes out on completion. Calls
// done exactly once with the breaker observation: nil on clean end or
// caller-cancellation, the mid-stream error otherwise.
func (r *Router) forwardStream(ctx context.Context, src <-chan provider.StreamEvent, first provider.StreamEvent, out chan<- provider.StreamEvent, done func(error)) {
	defer close(out)

	if !sendStreamEvent(ctx, out, first) {
		// Caller already gone — that's not a provider failure.
		done(nil)
		return
	}

	var lastErr error
	for ev := range src {
		if !sendStreamEvent(ctx, out, ev) {
			done(nil)
			return
		}
		if ev.Err != nil {
			lastErr = ev.Err
			// Provider closes channel after an error event; loop exits naturally.
		}
	}
	done(breakerObservation(lastErr))
}

func sendStreamEvent(ctx context.Context, out chan<- provider.StreamEvent, ev provider.StreamEvent) bool {
	select {
	case out <- ev:
		return true
	case <-ctx.Done():
		return false
	}
}

// extractRetryAfter pulls a Retry-After hint from a provider.UpstreamError
// chain. Returns 0 when no UpstreamError is present or RetryAfter is unset.
func extractRetryAfter(err error) time.Duration {
	var ue *provider.UpstreamError
	if errors.As(err, &ue) {
		return ue.RetryAfter
	}
	return 0
}

// defaultSleep blocks for d unless ctx is canceled first. Returns ctx.Err()
// on cancellation; nil otherwise. d <= 0 returns immediately.
func defaultSleep(ctx context.Context, d time.Duration) error {
	if d <= 0 {
		return nil
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}
