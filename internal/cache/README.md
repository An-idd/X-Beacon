# internal/cache

Response cache for `/v1/chat/completions`. Two layers planned:

- **Exact** (Week 9): byte-identical request → byte-identical response.
  Key = `cache:exact:<sha256(model + messages + sampling whitelist)>`,
  stored as JSON in Redis with a configurable TTL.
- **Semantic** (Week 10): cosine similarity over embeddings via
  RediSearch HNSW. Built on top of Exact.

## What's here today (Week 9)

| File | Purpose |
|------|---------|
| `exact.go` | `Exact` interface, `Key()` deriver, `RedisExact` implementation |

## Key derivation

`Key(*provider.ChatRequest)` hashes the canonical 5-tuple:

1. `model`
2. `messages` (chronological order is significant)
3. `temperature`, `top_p`, `max_tokens`, `stop` — the OpenAI sampling
   whitelist; everything else (`stream`, `user`, `n`, `Extra`) is
   excluded so logically-equivalent requests collide.

`stream` is intentionally excluded — Week 9 streams bypass the cache,
and Week 10 will replay cached non-stream responses as a synthetic
stream so the same key can serve both shapes.

## Anti-pollution rules

The cache itself stores whatever it's given. The chat handler is
responsible for filtering before `Set()`:

- HTTP 200 only
- `finish_reason == "stop"` (not `length` / `content_filter`)
- non-empty content
- `usage.prompt_tokens > 0`

`length`-truncated responses are deliberately **not** cached: they're
half-answers and clients typically retry with a larger `max_tokens`.

## Failure modes

`Get` / `Set` / `Delete` are best-effort. Backend errors are returned
verbatim (wrapped) so the middleware can log them; the chat path
treats any non-`ErrMiss` error as a miss and continues to the upstream.
