# internal/server/sse

Atomic SSE frame writer used by `/v1/chat/completions` streaming.

## Public surface

| Symbol | Purpose |
|--------|---------|
| `New(w http.ResponseWriter) (*Writer, error)` | Sets SSE headers and returns a writer. Errors with `ErrNotFlushable` if `w` doesn't implement `http.Flusher`. |
| `(*Writer).WriteData(payload []byte) error` | Emits `data: <payload>\n\n` and flushes. Caller marshals JSON. |
| `(*Writer).WriteComment(text string) error` | Emits `: <text>\n\n` and flushes. Used for keep-alive pings. |
| `(*Writer).StartHeartbeat(ctx, interval) func()` | Spawns a goroutine that sends `: keepalive` every `interval` until ctx cancels. Returned stop func is blocking — calling it guarantees the goroutine has exited. |
| `ErrNotFlushable` | Sentinel for the only failure mode of `New`. |

## Headers set by `New`

```
Content-Type: text/event-stream
Cache-Control: no-cache
Connection: keep-alive
X-Accel-Buffering: no   # nginx hint — disables response buffering
```

## Concurrency contract

All methods on `Writer` take an internal mutex. The heartbeat goroutine
shares that mutex with normal data writes, so frames cannot interleave.
Callers may write from multiple goroutines safely.

## Why the heartbeat stop func blocks

Calling `stop()` returns only after the goroutine has fully exited. This
matters because a deferred `stop()` in the chat-stream handler must
finish before the handler returns; otherwise a stray `WriteComment`
could race with `http.Server` shutting down the response.

## Not in scope

- HTTP/2 server push, gzip, chunked transfer wrangling — Go's
  `http.Server` does the right thing already.
- Generic SSE event-name + id semantics. The OpenAI dialect uses
  `data:`-only frames, which is all we need.
