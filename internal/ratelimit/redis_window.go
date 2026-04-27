package ratelimit

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"time"

	"github.com/redis/go-redis/v9"
)

// RedisWindow implements Limiter as a sliding-window counter backed by a
// Redis sorted set. Atomic via a single Lua script — no race between
// "count current entries" and "ZADD this request".
//
// Algorithm (per call):
//
//	1. ZREMRANGEBYSCORE  drop entries older than (now - window)
//	2. ZCARD             count remaining
//	3. if count + cost > limit: DENY
//	4. else: for i in 1..cost { ZADD now+i }
//	5. EXPIRE the key (window + slack) so empty buckets self-clean
//
// Each request adds `cost` distinct entries (suffixed by an index) so
// ZCARD genuinely reflects "in-flight cost". Score = now-with-suffix,
// the suffix appended via member-name uniqueness, score still sortable
// for ZREMRANGEBYSCORE.
//
// Suitable for cross-instance limits (the canonical use case for "global
// rate per API key"). For single-instance limits, MemoryBucket is
// cheaper and lower-latency.
type RedisWindow struct {
	rdb    redis.UniversalClient
	limit  int           // max events in the window
	window time.Duration // window size
	now    func() time.Time
}

// RedisWindowConfig configures the limiter.
type RedisWindowConfig struct {
	// Client is the Redis connection (already validated by main).
	Client redis.UniversalClient
	// Limit is the maximum allowed events per window. Must be >= 1.
	Limit int
	// Window is the rolling window duration. Must be > 0.
	Window time.Duration
}

// NewRedisWindow validates cfg and returns a RedisWindow.
func NewRedisWindow(cfg RedisWindowConfig) (*RedisWindow, error) {
	if cfg.Client == nil {
		return nil, errors.New("ratelimit: RedisWindowConfig.Client is required")
	}
	if cfg.Limit < 1 {
		return nil, errors.New("ratelimit: RedisWindowConfig.Limit must be >= 1")
	}
	if cfg.Window <= 0 {
		return nil, errors.New("ratelimit: RedisWindowConfig.Window must be > 0")
	}
	return &RedisWindow{
		rdb:    cfg.Client,
		limit:  cfg.Limit,
		window: cfg.Window,
		now:    time.Now,
	}, nil
}

// slidingWindowScript is the atomic check-and-record. Args:
//
//	KEYS[1] = sorted set key
//	ARGV[1] = now in nanoseconds
//	ARGV[2] = window in nanoseconds
//	ARGV[3] = limit
//	ARGV[4] = cost
//	ARGV[5] = expire seconds (window + slack)
//
// Returns: {allowed (1|0), remaining, retry_after_ms_when_denied (0 if allowed)}
//
// Implementation note: members must be unique within a single call, so
// we suffix with an index 1..cost. We could also include a counter or
// random nonce; the simple "now:i" form is enough since a single Lua
// invocation is atomic — no two scripts can collide on the same i.
var slidingWindowScript = redis.NewScript(`
local key      = KEYS[1]
local now      = tonumber(ARGV[1])
local window   = tonumber(ARGV[2])
local limit    = tonumber(ARGV[3])
local cost     = tonumber(ARGV[4])
local expire   = tonumber(ARGV[5])

-- Drop expired entries.
redis.call('ZREMRANGEBYSCORE', key, '-inf', now - window)

-- Current population.
local current = tonumber(redis.call('ZCARD', key))

if current + cost > limit then
  -- Compute retry_after = (oldest_score + window) - now.
  local oldest = redis.call('ZRANGE', key, 0, 0, 'WITHSCORES')
  local retry_ms = 0
  if oldest[2] ~= nil then
    local oldest_score = tonumber(oldest[2])
    retry_ms = math.ceil(((oldest_score + window) - now) / 1000000)
    if retry_ms < 0 then retry_ms = 0 end
  end
  return {0, math.max(0, limit - current), retry_ms}
end

-- Allow: insert <cost> distinct members. We use a Redis-side INCR so
-- two requests in the same nanosecond (or with a frozen test clock)
-- still produce unique member names. Score remains wall-clock now,
-- which is what ZREMRANGEBYSCORE keys on for window expiry.
local seq_key = key .. ':_seq'
for i = 1, cost do
  local seq = redis.call('INCR', seq_key)
  redis.call('ZADD', key, now, seq)
end

-- Self-cleaning TTL on both the sorted set and the seq counter.
redis.call('EXPIRE', key, expire)
redis.call('EXPIRE', seq_key, expire)

local remaining = limit - (current + cost)
if remaining < 0 then remaining = 0 end
return {1, remaining, 0}
`)

// Allow runs the Lua script and translates the result into a Decision.
func (r *RedisWindow) Allow(ctx context.Context, key string, cost int) (Decision, error) {
	if cost < 1 {
		cost = 1
	}
	now := r.now()

	expireSeconds := int64(r.window/time.Second) + 5 // slack so TTL > window

	res, err := slidingWindowScript.Run(ctx, r.rdb, []string{key},
		now.UnixNano(),
		r.window.Nanoseconds(),
		r.limit,
		cost,
		expireSeconds,
	).Result()
	if err != nil {
		return Decision{}, fmt.Errorf("ratelimit: redis script: %w", err)
	}

	arr, ok := res.([]any)
	if !ok || len(arr) != 3 {
		return Decision{}, fmt.Errorf("ratelimit: unexpected redis script result: %v", res)
	}
	allowed := toInt64(arr[0]) == 1
	remaining := int(toInt64(arr[1]))
	retryMs := toInt64(arr[2])

	d := Decision{
		Allowed:   allowed,
		Limit:     r.limit,
		Remaining: remaining,
		Reset:     now.Add(r.window),
	}
	if !allowed {
		d.RetryAfter = time.Duration(retryMs) * time.Millisecond
		d.Reset = now.Add(d.RetryAfter)
	}
	return d, nil
}

// toInt64 normalizes Redis script return values, which arrive as either
// int64 (Lua returned a number) or string (Lua returned a string). The
// Lua we wrote always returns numbers, but go-redis' parser is permissive.
func toInt64(v any) int64 {
	switch x := v.(type) {
	case int64:
		return x
	case int:
		return int64(x)
	case string:
		n, _ := strconv.ParseInt(x, 10, 64)
		return n
	}
	return 0
}
