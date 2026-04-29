# pkg/embedding

Vendor-neutral embedding-vector adapter for the gateway. Used by Week
10's semantic cache; safe to reuse for any future similarity-based
feature (log dedup, prompt clustering, etc.).

## Interface

```go
type Embedder interface {
    Embed(ctx context.Context, texts []string) ([][]float32, error)
    Dimensions() int
    Model() string
}
```

Batch-first by design: `Embed` always takes a slice and returns one
vector per input in input order. Single-text callers wrap as
`Embed(ctx, []string{text})` and read `vecs[0]`. The wrapping cost is
trivial; the upside is that bulk index-warm paths (Week 10+) reuse the
same call without inventing a second method.

The interface is intentionally **chat-agnostic**: the decision of
"what text to extract from a `*provider.ChatRequest`" lives in the
caller (semantic cache flatten function). Tuning that flatten strategy
is the single most likely thing we'll change after Week 10 ships, so
it's deliberately not bundled in here.

## Implementations

| File | Backend | Notes |
|------|---------|-------|
| `openai.go` | OpenAI `/v1/embeddings` | `text-embedding-3-small` default (1536 dims, ~$0.02 / 1M tokens). Supports the v3 `dimensions` parameter for index-size tuning. |

Errors are normalized through `internal/provider`'s sentinel set
(`ErrAuth`, `ErrRateLimited`, `ErrInvalidRequest`, `ErrUpstream`,
`ErrTimeout`) so callers can `errors.Is(...)` uniformly with chat
errors. Wraps come back as `*provider.UpstreamError` with provider
name `"openai-embeddings"`.

## Status — Week 9

The package is built but **not wired into the chat hot path**. Week
10 plugs it into the semantic cache lookup pipeline alongside
`internal/cache.SemanticIndex` (RediSearch HNSW).
