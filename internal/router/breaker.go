package router

import (
	"errors"
	"sync"
	"time"

	"github.com/sony/gobreaker/v2"
	"go.uber.org/zap"

	"github.com/An-idd/x-beacon/internal/provider"
)

// BreakerSettings configures the per-provider circuit breaker. Zero values
// fall back to DefaultBreakerSettings; the constructor merges them.
type BreakerSettings struct {
	// MaxRequests is the cap on probe requests allowed in the half-open
	// state before re-evaluating. 1 means "single test request decides".
	MaxRequests uint32

	// Interval is the rolling window over which Counts are kept while
	// the breaker is closed. 0 disables the auto-reset (counts grow
	// unbounded until a state change).
	Interval time.Duration

	// Timeout is how long the breaker stays open before transitioning
	// to half-open. Default 30s — short enough that a single dud
	// upstream doesn't black-hole traffic for minutes; long enough that
	// the upstream has a chance to recover.
	Timeout time.Duration

	// FailureRatio (0..1) is the failure rate above which the breaker
	// trips. Combined with MinRequests so a one-off blip on a quiet
	// provider doesn't open the circuit.
	FailureRatio float64

	// MinRequests is the floor on Counts.Requests before the trip
	// rule is even evaluated. Below this, the breaker stays closed
	// regardless of failure ratio.
	MinRequests uint32
}

// DefaultBreakerSettings returns conservative production defaults.
// Tuned to: trip after 5+ requests with ≥50% failure; reopen probe in 30s.
func DefaultBreakerSettings() BreakerSettings {
	return BreakerSettings{
		MaxRequests:  1,
		Interval:     60 * time.Second,
		Timeout:      30 * time.Second,
		FailureRatio: 0.5,
		MinRequests:  5,
	}
}

func (s *BreakerSettings) applyDefaults() {
	def := DefaultBreakerSettings()
	if s.MaxRequests == 0 {
		s.MaxRequests = def.MaxRequests
	}
	if s.Interval <= 0 {
		s.Interval = def.Interval
	}
	if s.Timeout <= 0 {
		s.Timeout = def.Timeout
	}
	if s.FailureRatio <= 0 {
		s.FailureRatio = def.FailureRatio
	}
	if s.MinRequests == 0 {
		s.MinRequests = def.MinRequests
	}
}

// breakerPool holds one TwoStepCircuitBreaker per provider name. Two-step
// is used (over Execute) so the same gating mechanism can wrap streaming
// calls in Step 6.4 — Allow() returns a `done` callback we invoke when
// the stream terminates.
type breakerPool struct {
	settings BreakerSettings
	logger   *zap.Logger

	mu sync.Mutex
	bs map[string]*gobreaker.TwoStepCircuitBreaker[any]
}

// newBreakerPool constructs a pool. Pools are created lazily per provider
// on first Allow().
func newBreakerPool(settings BreakerSettings, logger *zap.Logger) *breakerPool {
	settings.applyDefaults()
	return &breakerPool{
		settings: settings,
		logger:   logger,
		bs:       make(map[string]*gobreaker.TwoStepCircuitBreaker[any]),
	}
}

// allow returns a done callback (call with the request's resulting err) and
// either nil (request may proceed) or a non-nil err (breaker is open or
// half-open quota exceeded). Callers translate non-nil err into a
// failover-eligible signal (treated as ErrUnavailable upstream).
func (p *breakerPool) allow(name string) (func(err error), error) {
	cb := p.get(name)
	return cb.Allow()
}

// state exposes the current breaker state for tests / debug logging.
func (p *breakerPool) state(name string) gobreaker.State {
	cb := p.get(name)
	return cb.State()
}

func (p *breakerPool) get(name string) *gobreaker.TwoStepCircuitBreaker[any] {
	p.mu.Lock()
	defer p.mu.Unlock()
	if cb, ok := p.bs[name]; ok {
		return cb
	}
	settings := p.settings
	logger := p.logger
	cb := gobreaker.NewTwoStepCircuitBreaker[any](gobreaker.Settings{
		Name:        name,
		MaxRequests: settings.MaxRequests,
		Interval:    settings.Interval,
		Timeout:     settings.Timeout,
		ReadyToTrip: func(counts gobreaker.Counts) bool {
			if counts.Requests < settings.MinRequests {
				return false
			}
			ratio := float64(counts.TotalFailures) / float64(counts.Requests)
			return ratio >= settings.FailureRatio
		},
		OnStateChange: func(name string, from gobreaker.State, to gobreaker.State) {
			if logger != nil {
				logger.Warn("circuit breaker state changed",
					zap.String("provider", name),
					zap.String("from", from.String()),
					zap.String("to", to.String()))
			}
		},
	})
	p.bs[name] = cb
	return cb
}

// isBreakerError reports whether err came from gobreaker's Allow() rather
// than the underlying provider call. The router treats these as
// failover-eligible signals (akin to provider.ErrUnavailable).
func isBreakerError(err error) bool {
	return errors.Is(err, gobreaker.ErrOpenState) || errors.Is(err, gobreaker.ErrTooManyRequests)
}

// breakerObservation translates a provider call error into the breaker's
// view of "failure." nil → counted as success (call returned cleanly OR
// only had a request-shape problem). Non-nil → counted as a real upstream
// failure that should pressure the breaker toward open.
//
// 4xx-class errors (auth, invalid request, context length) are NOT
// counted: a healthy upstream is rejecting bad input. Tripping the
// breaker on those would penalize the next innocent caller.
func breakerObservation(callErr error) error {
	if callErr == nil {
		return nil
	}
	if provider.IsRetryable(callErr) {
		return callErr
	}
	return nil
}
