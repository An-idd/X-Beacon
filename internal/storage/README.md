# internal/storage

PostgreSQL connection pool + schema migrations.

## Public surface

| Symbol | Purpose |
|--------|---------|
| `Config` | DSN + pool sizing knobs (defined locally; main maps from `config.DatabaseConfig`) |
| `NewPool(ctx, cfg) (*Pool, error)` | Lazy pool — succeeds even if DB is down; first connection happens on use |
| `(*Pool).Ping(ctx) error` | Round-trip used by `/readyz` |
| `(*Pool).Close()` | Releases the pool; nil-safe |
| `MigrateUp(dsn)` / `MigrateDown(dsn)` / `MigrateVersion(dsn)` | Schema management; `xbctl migrate up` invokes these |

## Migrations

`migrations/*.sql` are embedded into the binary via `//go:embed`. Naming
follows `golang-migrate` conventions:

```
000001_create_api_keys.up.sql
000001_create_api_keys.down.sql
```

Adding a migration:

1. Pick the next zero-padded number.
2. Write `<n>_<slug>.up.sql` + `.down.sql` next to the existing files.
3. The embed directive picks them up; rebuild the binary.
4. Run `xbctl migrate up` (Step 4.5) or, for local dev,
   `make migrate-up` against the disk copy.

## Schema

### `api_keys` (migration 000001)

```sql
CREATE TABLE api_keys (
    id           TEXT        PRIMARY KEY,
    key_hash     BYTEA       NOT NULL UNIQUE,   -- sha256 of secret
    name         TEXT        NOT NULL,
    scopes       JSONB       NOT NULL DEFAULT '{}',
    revoked_at   TIMESTAMPTZ,
    created_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
    last_used_at TIMESTAMPTZ
);

CREATE INDEX idx_api_keys_active_hash
    ON api_keys (key_hash)
    WHERE revoked_at IS NULL;
```

- **`key_hash` is the only secret-derived value persisted** — raw secrets never reach the DB.
- **`scopes JSONB`** is reserved for Week 5 rate-limit classes / Week 7 billing attribution. Current code ignores the payload.
- **Logical revocation** (`revoked_at IS NOT NULL`) — keeps audit trails intact and lets the cache (Step 4.4) negative-cache revoked keys.

## Testing

- Unit: pool construction, DSN parsing, embed integrity (`go test ./internal/storage`).
- Integration: gated by `XBEACON_TEST_DSN`. Spin up postgres via `make docker-up` then:

```bash
XBEACON_TEST_DSN=postgres://xbeacon:xbeacon@localhost:5432/xbeacon?sslmode=disable \
    go test ./internal/storage/...
```

Integration test exercises Up → Version → table existence check → Down round-trip.

## Why no ORM

CLAUDE.md prohibits ORMs in this repo. Hand-written SQL + pgx gives us
prepared-statement caching for free, predictable error mapping (pgconn
error codes → gateway sentinels), and zero magic at a query boundary.
