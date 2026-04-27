package auth

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/redis/go-redis/v9"
	"go.uber.org/zap"
	"golang.org/x/sync/singleflight"
)

// CachedAuthenticator wraps an inner Authenticator with a Redis-backed
// lookup cache. It serves three goals:
//
//  1. Cut the per-request DB round-trip on hits (target: P99 < 10ms).
//  2. Negative-cache invalid credentials so a flood of bogus tokens
//     can't DDoS the database.
//  3. Coalesce concurrent misses for the same key into a single inner
//     Authenticate (singleflight) — no thundering herd on cache eviction.
//
// Cache layout in Redis:
//
//	key:   auth:k:<sha256-hex>   (hex for redis-cli debuggability)
//	value: JSON-marshalled *Principal (positive) or "null" (negative)
//
// Redis errors are treated as cache misses (fail-open). Redis is a cache,
// not the source of truth — degrading to "go ask the DB" is the safe
// behavior. The error is logged at debug level so misconfigured clusters
// surface in observability without spamming production logs.
//
// Side effect: on a cache hit, last_used_at is NOT bumped on the inner
// Authenticator. The bookkeeping is sacrificed for the 99%-write
// reduction; consequently last_used_at semantics shift from "exact last
// auth" to "last cache miss for this key" (precision ~ posTTL). This is
// documented in [internal/auth/README.md].
type CachedAuthenticator struct {
	inner  Authenticator
	rdb    redis.UniversalClient
	posTTL time.Duration
	negTTL time.Duration
	sf     singleflight.Group
	logger *zap.Logger
}

// NewCached wraps inner with a Redis cache. posTTL governs how long a
// successful lookup is held; negTTL is the (typically much shorter)
// retention for invalid credentials so a freshly-issued key isn't
// shadowed by a stale "not found" entry.
//
// posTTL=0 / negTTL=0 disable that branch — handy for tests that want
// to verify a single roundtrip without measuring stale-window behavior.
func NewCached(inner Authenticator, rdb redis.UniversalClient, posTTL, negTTL time.Duration, logger *zap.Logger) *CachedAuthenticator {
	if logger == nil {
		logger = zap.NewNop()
	}
	return &CachedAuthenticator{
		inner:  inner,
		rdb:    rdb,
		posTTL: posTTL,
		negTTL: negTTL,
		logger: logger,
	}
}

// Authenticate looks up the cache, falls back to inner on miss, and
// stores the result for future calls. The empty-key shortcut runs first
// to keep ErrMissingCredentials free of a Redis roundtrip.
func (c *CachedAuthenticator) Authenticate(ctx context.Context, key string) (*Principal, error) {
	if key == "" {
		return nil, ErrMissingCredentials
	}
	if c.inner == nil {
		return nil, errors.New("auth: cached authenticator has no inner")
	}

	cacheKey := "auth:k:" + hashKeyHex(key)

	if hit, ok, err := c.read(ctx, cacheKey); ok {
		// Errors from the cache parse layer are non-fatal but worth a debug
		// line so a corrupt entry surfaces in test runs.
		if err != nil {
			c.logger.Debug("auth cache value malformed; treating as miss",
				zap.String("cache_key", cacheKey), zap.Error(err))
		} else {
			return hit.principal, hit.err
		}
	}

	// singleflight: any concurrent callers with the same key see one
	// inner.Authenticate and share its outcome.
	res, err, _ := c.sf.Do(cacheKey, func() (any, error) {
		p, innerErr := c.inner.Authenticate(ctx, key)
		c.write(ctx, cacheKey, p, innerErr)
		// Wrap into a sentinel struct so singleflight's value carries both
		// principal and error consistently for shared callers.
		return cacheHit{principal: p, err: innerErr}, nil
	})
	if err != nil {
		return nil, err
	}
	hit := res.(cacheHit)
	return hit.principal, hit.err
}

// cacheHit is the value type stored in singleflight and returned from
// the cache read. Encoding errors and inner errors are kept separate so
// callers can distinguish "we have a definitive negative cache" from
// "we need to redo the lookup".
type cacheHit struct {
	principal *Principal
	err       error
}

// read returns (hit, ok=true, parseErr=nil) on a clean cache hit;
// (zero, false, nil) on miss or Redis error; (zero, true, parseErr) when
// the cached value is unparseable.
func (c *CachedAuthenticator) read(ctx context.Context, cacheKey string) (cacheHit, bool, error) {
	raw, err := c.rdb.Get(ctx, cacheKey).Bytes()
	switch {
	case errors.Is(err, redis.Nil):
		return cacheHit{}, false, nil
	case err != nil:
		// Redis outage: fail open (treat as miss), do not propagate.
		c.logger.Debug("auth cache read failed; degrading to inner",
			zap.String("cache_key", cacheKey), zap.Error(err))
		return cacheHit{}, false, nil
	}

	// "null" → negative cache (invalid credentials).
	if len(raw) == 4 && string(raw) == "null" {
		return cacheHit{err: ErrInvalidCredentials}, true, nil
	}

	var p Principal
	if jsonErr := json.Unmarshal(raw, &p); jsonErr != nil {
		return cacheHit{}, true, jsonErr
	}
	return cacheHit{principal: &p}, true, nil
}

// write stores the result of an inner.Authenticate call into the cache.
// Wrapped DB errors (i.e. errors that aren't ErrInvalidCredentials but
// also aren't success) are NOT cached — they may be transient and we
// want the next call to retry the inner.
func (c *CachedAuthenticator) write(ctx context.Context, cacheKey string, p *Principal, innerErr error) {
	switch {
	case innerErr == nil:
		if c.posTTL <= 0 {
			return
		}
		body, err := json.Marshal(p)
		if err != nil {
			c.logger.Debug("auth cache encode failed", zap.Error(err))
			return
		}
		if err := c.rdb.Set(ctx, cacheKey, body, c.posTTL).Err(); err != nil {
			c.logger.Debug("auth cache write (positive) failed", zap.Error(err))
		}

	case errors.Is(innerErr, ErrInvalidCredentials):
		if c.negTTL <= 0 {
			return
		}
		if err := c.rdb.Set(ctx, cacheKey, "null", c.negTTL).Err(); err != nil {
			c.logger.Debug("auth cache write (negative) failed", zap.Error(err))
		}

	default:
		// Anything else (DB outage, ErrMissingCredentials, wrapped errors)
		// is intentionally not cached so transient failures don't poison
		// future lookups.
	}
}

// Inner returns the wrapped Authenticator. Useful in tests and when the
// caller needs to bypass cache (e.g. xbctl wanting a fresh DB read).
func (c *CachedAuthenticator) Inner() Authenticator { return c.inner }

// _ ensures the type signature stays in sync with the contract.
var _ Authenticator = (*CachedAuthenticator)(nil)
