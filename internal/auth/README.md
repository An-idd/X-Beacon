# internal/auth

API key authentication.

## Status

| Backend | Status | When to use |
|---------|--------|-------------|
| `PostgresAuthenticator` | **production** (Step 4.3) | Always, in real deployments |
| `StaticAuthenticator` | **test helper only** (downgraded in Step 4.3) | Unit tests, smoke tests, anything that shouldn't depend on a live DB |
| `CachedAuthenticator` | **production** (Step 4.4) | Wraps Postgres with Redis cache + negative cache + singleflight |

## Public surface

| Symbol | Purpose |
|--------|---------|
| `Authenticator` | Interface — `Authenticate(ctx, key) (*Principal, error)` |
| `Principal` | Identifies the API consumer — `ID`, `Name` (no secrets) |
| `WithPrincipal` / `PrincipalFrom` | Context plumbing used by the auth middleware |
| `ErrMissingCredentials` / `ErrInvalidCredentials` | Sentinels for middleware classification |
| `NewPostgres(pool)` | DB-backed Authenticator, queries `api_keys` |
| `NewCached(inner, redis, posTTL, negTTL, logger)` | Wraps any inner with the Redis cache decorator |
| `NewStatic(entries)` | **Test helper** — in-memory, hash-on-load, never used in main wiring |

## Postgres backend

Single SQL statement does the lookup AND the `last_used_at` refresh:

```sql
UPDATE api_keys
   SET last_used_at = now()
 WHERE key_hash = $1
   AND revoked_at IS NULL
RETURNING id, name
```

The partial index `idx_api_keys_active_hash` (only over non-revoked rows)
keeps this an index-scan even as the audit history grows.

`pgx.ErrNoRows` → `ErrInvalidCredentials`. Any other DB error is wrapped
and bubbles up; the auth middleware maps unwrapped errors to 500.

`Authenticate` does not check the DB pool's health on every call — that's
the readiness probe's job (Step 4.6 `/readyz`). If the DB goes down
mid-traffic, in-flight queries fail fast and return 500; cached principals
(Step 4.4) keep working until their TTL expires.

## Cache layer (Step 4.4)

`CachedAuthenticator` wraps any inner Authenticator with three behaviors:

1. **Positive cache** (default 60s TTL). Successful lookups skip both the
   DB roundtrip AND the `last_used_at` update on subsequent hits.
2. **Negative cache** (default 5s TTL). `ErrInvalidCredentials` is cached
   briefly so a flood of bogus tokens can't DDoS the DB. The TTL is
   intentionally short so a freshly-issued key isn't shadowed by a stale
   "not found" entry.
3. **Singleflight** (`golang.org/x/sync/singleflight`). N concurrent
   misses for the same key collapse into one inner call — no thundering
   herd at cache-eviction boundaries.

### Cache layout

```
key:   auth:k:<sha256-hex>
value: <Principal JSON>   (positive)
       null               (negative)
```

Hex keys make `redis-cli KEYS auth:k:*` debuggable at the cost of 64 bytes
per key (deemed worthwhile vs. binary).

### Side effect: `last_used_at` precision

Cache hits do NOT bump `last_used_at` on the inner Postgres backend.
Net write reduction is ~99% under realistic cache-hit rates, but the
column's semantics shift from "exact last auth" to **"last cache miss
for this key"** (precision = `posTTL`, default 60s). Operators who
need fine-grained access timestamps must either lower `posTTL` or sample
auth logs.

### Failure modes

- **Redis unreachable**: cache reads fail silently; cache writes no-op;
  every request hits the inner Authenticator. The gateway stays up.
- **Cache value malformed** (manual `SET` of garbage by an admin, or a
  schema-incompatible upgrade): treated as miss, replaced on next write.
- **Inner backend error** (DB outage, etc.): NOT cached. Transient
  failures must retry, not poison.

## Static backend (test helper)

Kept for `test/smoke_test.go` and middleware tests that need an
Authenticator without spinning up Postgres. Hash-on-load semantics still
apply — raw secrets exit memory after `NewStatic` returns.

If you find yourself reaching for `NewStatic` in production code, that's
a mistake: it cannot be revoked, scoped, or rotated.

## Hash function

Both backends use SHA-256:

- `hashKeyBytes(secret) []byte` — 32 raw bytes for the Postgres BYTEA column
- `hashKeyHex(secret) string` — 64-char lowercase hex for the in-memory map (Go map keys can't be byte slices)

Same digest, just different envelopes.
