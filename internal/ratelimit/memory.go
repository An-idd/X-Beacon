package ratelimit

import (
	"context"
	"errors"
	"time"

	expirable "github.com/hashicorp/golang-lru/v2/expirable"
	"golang.org/x/time/rate"
)

// MemoryBucket implements Limiter via x/time/rate.Limiter, one bucket per
// composed key. Buckets live in an LRU bounded by MaxKeys + IdleTTL so a
// flood of unique keys (per-IP enumeration, etc.) can't blow out memory.
//
// Eviction trade-off: when a key's bucket is dropped, the next request
// on that key gets a fresh full bucket. We accept the looseness — the
// alternative (long-term retention of cold buckets) is worse for memory.
//
// Suitable for single-instance deployments; for cross-instance limits
// use RedisWindow (Step 5.3).
type MemoryBucket struct {
	rate  rate.Limit
	burst int
	cache *expirable.LRU[string, *rate.Limiter]
	now   func() time.Time // injected for tests
}

// MemoryBucketConfig configures the bucket. Zero values for MaxKeys /
// IdleTTL fall back to defaults; Rate and Burst are required.
type MemoryBucketConfig struct {
	// Rate is the steady-state allowance in tokens per second. Must be > 0.
	Rate rate.Limit

	// Burst is the maximum tokens a key can accumulate. Must be >= 1.
	Burst int

	// MaxKeys caps the LRU. 0 → 100_000 (Week 5 carry-over G).
	MaxKeys int

	// IdleTTL evicts buckets unused for this long. 0 → 1h.
	IdleTTL time.Duration
}

// NewMemoryBucket validates cfg and returns a Limiter.
func NewMemoryBucket(cfg MemoryBucketConfig) (*MemoryBucket, error) {
	if cfg.Rate <= 0 {
		return nil, errors.New("ratelimit: MemoryBucketConfig.Rate must be > 0")
	}
	if cfg.Burst < 1 {
		return nil, errors.New("ratelimit: MemoryBucketConfig.Burst must be >= 1")
	}
	if cfg.MaxKeys == 0 {
		cfg.MaxKeys = 100_000
	}
	if cfg.IdleTTL == 0 {
		cfg.IdleTTL = time.Hour
	}

	cache := expirable.NewLRU[string, *rate.Limiter](cfg.MaxKeys, nil, cfg.IdleTTL)
	return &MemoryBucket{
		rate:  cfg.Rate,
		burst: cfg.Burst,
		cache: cache,
		now:   time.Now,
	}, nil
}

// Allow consults (and decrements when allowed) the per-key bucket.
//
// On allow: Decision carries the post-decrement token count as Remaining
// and projects Reset = now + (deficit / rate).
//
// On deny: ReserveN gives us the precise wait time without consuming
// tokens; the reservation is canceled so the deny is observation-only.
// (AllowN already decided not to take the tokens, but its return doesn't
// expose retry-after; ReserveN is the exposed shortest path.)
func (m *MemoryBucket) Allow(_ context.Context, key string, cost int) (Decision, error) {
	if cost < 1 {
		cost = 1
	}
	now := m.now()

	lim, ok := m.cache.Get(key)
	if !ok {
		lim = rate.NewLimiter(m.rate, m.burst)
		// Add returns whether eviction occurred; we don't act on it.
		m.cache.Add(key, lim)
	}

	if lim.AllowN(now, cost) {
		// Reset projects when the bucket would be full again.
		tokensLeft := int(lim.TokensAt(now))
		deficit := m.burst - tokensLeft
		var reset time.Time
		if deficit > 0 && m.rate > 0 {
			reset = now.Add(time.Duration(float64(deficit)/float64(m.rate)) * time.Second)
		} else {
			reset = now
		}
		return Decision{
			Allowed:   true,
			Limit:     m.burst,
			Remaining: tokensLeft,
			Reset:     reset,
		}, nil
	}

	// Denied. ReserveN tells us when `cost` tokens become available; we
	// cancel immediately so the rejection has no side effects on the bucket.
	res := lim.ReserveN(now, cost)
	delay := res.DelayFrom(now)
	res.CancelAt(now)

	return Decision{
		Allowed:    false,
		Limit:      m.burst,
		Remaining:  int(lim.TokensAt(now)),
		Reset:      now.Add(delay),
		RetryAfter: delay,
	}, nil
}

// Len returns the current LRU population. Useful for metrics + tests.
func (m *MemoryBucket) Len() int { return m.cache.Len() }
