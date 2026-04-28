# pkg/tokenizer

Token counting for prompt + completion accounting. Two implementations
behind one tight interface; `Selector` picks per model id.

## Public surface

| Symbol | Purpose |
|--------|---------|
| `Tokenizer` | `Family() / CountMessages([]Message) / CountText(string)` |
| `NewOpenAI()` | cl100k_base BPE (faithful) |
| `NewAnthropic()` | cl100k × 1.15 (approximation) |
| `NewSelector()` | model id → tokenizer router |

## Tokenizer choice

- **OpenAI / DeepSeek / unknown** → `cl100k_base` via
  [`tiktoken-go/tokenizer`](https://github.com/tiktoken-go/tokenizer).
  Vocab is **embedded in the binary** — no network fetch, no cold-start
  cost. Counts match OpenAI's official `tiktoken` to the token.
- **Anthropic** (`claude*` prefix, case-insensitive) → cl100k count
  scaled by **1.15×**. Anthropic doesn't ship a public tokenizer for
  arbitrary text; the factor comes from public model-card numbers and
  internal benchmarks. Documented as **approximate**; billing reconciles
  against the upstream's `usage` block when available.

## Message overhead

`CountMessages` applies OpenAI's published cookbook overhead:

```
total = primingOverhead (3)
for each message:
  total += perMessageOverhead (3)
  total += tokens(role)
  total += tokens(content)
  if name != "":
    total += tokens(name)
```

This matches what the upstream charges for prompt accounting. The
trailing assistant priming (3 tokens) is paid once per request; we add
it unconditionally because `/v1/chat/completions` is the only consumer.

## Failure modes

`CountText` falls back to a char/4 heuristic if the BPE encoder errors
(unreachable in practice — BPE is exhaustive over UTF-8 — but the
fallback prevents silent zeros entering billing).

## When to use what

- **Pre-call (Week 5 ratelimit cost)**: `CountMessages(req.Messages)`
  for the rate-limit token budget. Was hardcoded to 1; now reflects
  real prompt size.
- **Streaming completion accounting**: `CountText(chunk.Delta.Content)`
  accumulated across the stream. Use this only when the upstream's
  terminal `usage` chunk is missing — providers that emit it (Anthropic;
  recent OpenAI) should win.
- **Non-stream completion**: prefer `resp.Usage` from the provider; fall
  back to `CountText(choice.Message.Content)` if absent.
