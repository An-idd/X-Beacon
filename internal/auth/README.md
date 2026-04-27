# internal/auth

API key authentication.

## Status

- **v1 (Week 3, current)**: static, in-memory key table loaded from
  `configs/auth.yaml`. Hash-on-load, never persists raw secrets.
- **v2 (Week 4, planned)**: Postgres-backed table with Redis cache. Same
  `Authenticator` interface — only the storage swaps.

## Public surface

| Symbol | Purpose |
|--------|---------|
| `Authenticator` | Interface: `Authenticate(ctx, key) (*Principal, error)` |
| `Principal` | Identifies the API consumer behind a request — `ID`, `Name` (no secrets) |
| `WithPrincipal` / `PrincipalFrom` | Context plumbing used by the auth middleware and handlers |
| `ErrMissingCredentials` / `ErrInvalidCredentials` | Sentinels for middleware classification |
| `NewStatic(entries)` | Build a static authenticator (used by tests + `Load`) |
| `Load(path)` | Read `auth.yaml` → `*StaticAuthenticator` |

## YAML schema (`configs/auth.yaml`)

```yaml
keys:
  - id: dev-local           # stable, non-secret identifier
    name: "Local development"  # display name, optional
    secret: ${XBEACON_DEV_KEY:-sk-local-dev}  # ${VAR} or ${VAR:-default}
```

Env-var expansion is shared with `providers.yaml` via
`internal/envexpand`. An expanded-empty secret fails Load — silently
dropping a principal would be worse than refusing to start.

## Design notes

- **Hash on load, never on lookup**: the static table indexes by
  SHA-256(secret); raw secrets exit memory after `NewStatic` returns.
  `Authenticate` hashes the inbound key once, then does a map lookup —
  no plaintext compare, no per-secret branching that could leak via
  timing.
- **Duplicate-secret detection at startup**: two principals sharing a
  secret would silently mask one. `NewStatic` rejects this so misconfig
  is caught at boot, not at first auth attempt.
- **Errors are aggregated**: `NewStatic` collects all entry errors via
  `errors.Join` (same idiom as registry.Load), so a single fix-pass on
  auth.yaml clears the entire list.
- **Backend errors are distinct from invalid credentials**: middleware
  maps `ErrInvalidCredentials` → 401, anything else → 500. Week 4's DB
  outage path uses this distinction.

## Testing

`internal/auth` is hermetic — no network, no goroutines. Tests cover
constructor validation, env expansion, and the three `Authenticate`
outcomes. Middleware tests live in
[`internal/server/middleware/auth_test.go`](../server/middleware/auth_test.go)
and use a `fakeAuthn` to keep them decoupled from the static
implementation's hashing.
