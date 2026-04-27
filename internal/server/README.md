# internal/server

HTTP routing surface for the gateway.

## Responsibility

This package owns the chi router and route handlers. It does **not** load
config, construct providers, or manage process lifecycle — those are
`cmd/gateway`'s job. Server consumes constructed dependencies via `Deps`
and returns an `http.Handler`.

```
main.go (cmd/gateway)
  ├─ load config / logger / metrics / tracer / registry
  └─ server.New(Deps{...}) → *Server
                                └─ Handler() → mounted on http.Server
```

## Routes (Step 3.1)

| Method | Path | Handler | Purpose |
|--------|------|---------|---------|
| GET | `/healthz` | `healthzHandler` | Liveness — always 200 if process is up |
| GET | `/v1/models` | `modelsHandler` | OpenAI-compatible model catalog |
| GET | `/metrics` (configurable path) | promhttp | Prometheus scrape, mounted iff `MetricsEnabled` |

Future steps add:
- 3.2 Recovery / RequestID / Logging / Tracing middleware
- 3.3 Auth middleware on `/v1/*`
- 3.4 `/v1/chat/completions` non-streaming
- 3.5 `/v1/chat/completions` streaming + SSE writer
- 3.6 `/readyz` (Phase 0 carry-over) once DB/Redis exist (Week 4)

## Dependency contract

`Deps` is the single seam between main and server. Adding a new dependency
(e.g. `Authenticator` in 3.3, `RateLimiter` in Week 5) extends this struct;
the call site in main.go stays narrow.

`New` validates that required deps are non-nil. `MetricsReg` is required
only when `MetricsEnabled=true` — keeps tests that don't care about
metrics one line shorter.

## Testing notes

- `/v1/models` populated case uses `registry.Load` with a temp YAML file
  rather than a fake provider, because the registry package doesn't expose
  an in-memory seed constructor. Adding one would be a registry-internal
  decision; for now the temp-file approach is stable and reflects how
  production seeds the registry.
- The `loadRegistry` helper (providers.yaml-absent → warn-and-empty) lives
  in `cmd/gateway/handlers.go` rather than here. It encodes a startup-time
  policy (zero-config dev experience), not server logic.
