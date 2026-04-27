# internal/ratelimit

Rate-limiting primitives. Two backends, one composition layer.

## Public surface

| Symbol | Purpose |
|--------|---------|
| `Limiter` | Interface — `Allow(ctx, key, cost) (Decision, error)` |
| `Decision` | Outcome of a check: Allowed / Limit / Remaining / Reset / RetryAfter / Rule |
| `KeyBy`, `KeyContext` | Per-request dimension bag (api_key / model) |
| `Rule` | Limiter + KeyBy + Name; composes the canonical Redis key |
| `Multi` | First-deny-wins aggregator; on pass returns the tightest Decision |
| `MemoryBucket` | Token bucket via `golang.org/x/time/rate` + LRU |
| `RedisWindow` | Atomic sliding window via Lua + sorted set |
| `Build([]RuleConfig, redis)` | YAML config → []*Rule |

## Algorithms

### `memory_bucket` (single-instance)

Wraps `rate.Limiter` per composed key. LRU bounds memory (default 100k
keys, 1h idle TTL). When a key is evicted, the next request gets a fresh
full bucket — accept the looseness; the alternative (long retention of
cold buckets) is worse for memory.

### `redis_window` (cross-instance)

One sorted set per composed key. Single Lua script does:

```
ZREMRANGEBYSCORE key  -inf  (now - window)   -- drop expired
ZCARD key                                     -- count remaining
if count + cost > limit:
  read oldest score → compute retry_after
  return DENY
for i in 1..cost:
  seq = INCR key:_seq                         -- ensures member uniqueness
  ZADD key now seq
EXPIRE key window+5
EXPIRE key:_seq window+5
return ALLOW
```

The INCR sequence guarantees member uniqueness even when two requests
share a nanosecond timestamp (or, in tests, a frozen clock). Score
remains the wall-clock so window expiry math stays correct.

## Composition

`Multi.Check` runs every Rule in order:

- First **deny** wins; remaining rules don't run (early-exit).
- All **pass** → Decision from the rule with smallest `Remaining`.
  This is the bottleneck the client is closest to — the most useful
  number to surface as `X-RateLimit-Remaining`.

The middleware (`internal/server/middleware/ratelimit.go`) calls
`Multi.Check` once per request, fails open on backend errors, and emits
429 + `Retry-After` + the `X-RateLimit-*` header trio.

## YAML schema

See [configs/config.example.yaml](../../configs/config.example.yaml)'s
`rate_limits:` block for the complete shape with examples.

## Streaming

Rate limits apply only at request entry. Once `/v1/chat/completions`
streaming starts (Step 3.5), no further rate checks fire — matches
OpenAI's observed behavior; mid-stream rejection truncates output and
breaks SDK parsers.

## Backend failures

- **Redis outage (redis_window)**: middleware logs at warn and **fails
  open** — request passes through. Failing closed during a Redis tickle
  amounts to "Redis tickle = full outage".
- **Memory bucket**: no failure modes (in-process map).

## Cost (Week 5)

`cost` is hard-coded to 1 in the middleware. Week 7+ may pass token
counts when billing is online — the Limiter contract already accepts
arbitrary `cost`, so backends are forward-compatible.
