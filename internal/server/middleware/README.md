# internal/server/middleware

HTTP middleware chain that wraps every gateway request.

## Mounting order

Outer → inner, set in [`server.go`](../server.go). Order matters; do not
reorder without re-reading the rationale below.

```
Recovery  → catches panics from anything below (incl. other middleware)
RequestID → makes req_id available to Tracing / Logging / handlers
Tracing   → opens an OTel span; req_id is already in ctx so logs/traces correlate
Logging   → innermost; runs after handler returns to capture final status / latency
```

## Files

| File | Middleware | Notes |
|------|------------|-------|
| `requestid.go` | `RequestID()` | Reuses inbound `X-Request-ID` (≤128 chars) or generates UUIDv4 |
| `recovery.go` | `Recovery(logger)` | Logs panic + stack at error level; writes 500 JSON; re-panics on `http.ErrAbortHandler` |
| `tracing.go` | `Tracing(serviceName)` | Thin wrap over `otelhttp.NewHandler`. Span name = `<service>.http` |
| `logging.go` | `Logging(logger, opts)` | One zap line per request; level = info (<400) / warn (4xx) / error (5xx). `SkipPaths` suppresses noisy paths |

## Public helpers

- `RequestIDFrom(ctx) string` — pull the request ID stored by `RequestID()`. Empty string if absent.
- `HeaderRequestID` — the wire header name (`X-Request-ID`).

## Design notes

- **No chi/middleware**: chi's `Recoverer` writes to stdlib log, `RequestID`
  is an integer counter, `Logger` isn't zap-aware. Writing four short
  middlewares is cheaper than wrapping their output.
- **`loggingResponseWriter` exposes `Flush`**: required for SSE
  (`/v1/chat/completions` streaming, Step 3.5). If we ever need
  `Hijack`/`CloseNotify`, add them at that point.
- **Tracing span name is fixed**: chi's route pattern isn't known until the
  mux dispatches, after the span is already created. Renaming it later via
  `trace.SpanFromContext(ctx).SetName(...)` from inside the handler is
  cheap and can be added when route-level granularity matters (Phase 2 /
  Week 8 observability sweep).
- **Skip paths default to `/healthz` + `/metrics`** in server.go: probes
  and Prom scrapes would otherwise dominate access logs.
