# internal/router

The decision layer between HTTP handlers and provider adapters. Owns the
cross-call concerns — retry, fail-over (Step 6.2), circuit breaking (Step
6.3) — so that provider adapters stay stateless and lookup-only.

## Public surface

| Symbol | Purpose |
|--------|---------|
| `Router` | Orchestrates `ChatCompletion` (and stream — Step 6.4) under a `RetryPolicy` |
| `New(resolver, policy, logger, opts...)` | Constructor; `resolver` is typically `*registry.Registry` |
| `ModelResolver` | The `ResolveModel(model) (Provider, error)` interface — small and consumer-defined so tests can stub |
| `RetryPolicy` | `MaxRetries / MaxTotal / BaseBackoff / MaxBackoff` |
| `DefaultPolicy()` | Carry-over values: 2 retries / 30s total / 100ms base / 5s cap |
| `WithClock`, `WithSleep`, `WithRandom` | Test-only injectors for deterministic timing/jitter |

## Retry algorithm

Loop semantics for `ChatCompletion`:

1. Resolve model → provider via the registry; resolution failure is **not**
   retried — it's a 400 model_not_found at the HTTP layer.
2. Call provider; on `nil` error return.
3. On `provider.IsRetryable(err) == false`, return the error as-is.
4. On retryable error, compute the next delay:
   - If the upstream sent `Retry-After` (parsed into
     `provider.UpstreamError.RetryAfter`), honor it verbatim — no jitter.
     Fighting the explicit signal just gets us rate-limited again.
   - Otherwise, full-jitter exponential:
     `delay = rand[0,1) × min(MaxBackoff, BaseBackoff × 2^(attempt−1))`.
     Pure exponential without jitter creates a thundering herd whenever
     multiple clients hit the same upstream blip in lockstep.
5. Check `MaxTotal`: if `elapsed + delay > MaxTotal`, abort with the last
   error rather than sleeping.
6. Sleep (interruptible by ctx); cancellation returns `ctx.Err()`.

`MaxRetries` and `MaxTotal` are AND-style budgets — the loop stops when
either is exhausted. The pair guards against both fast-failing storms
(count) and slow-failing backlog (time).

## Streaming

Stream support is added in Step 6.4. Retries will only be permitted before
the first chunk is emitted; once the SDK has begun parsing increments,
silently switching to a different provider produces "first half OpenAI /
second half Anthropic" hallucination splices.

## Testability

`Router` accepts opaque function pointers for `now`, `sleep`, and `random`,
so retry-loop tests assert exact backoff sequences without real sleep
(table-driven; `random` returns 0.5 → 50 % envelope; clock advances by the
amount each `sleep` call requested).
