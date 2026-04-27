-- api_keys is the gateway's authentication table. Each row is one
-- bearer token; the secret itself is never persisted, only its
-- SHA-256 hash. Revocation is logical (revoked_at IS NOT NULL) so
-- audit trails stay intact.
CREATE TABLE api_keys (
    id           TEXT        PRIMARY KEY,
    key_hash     BYTEA       NOT NULL UNIQUE,
    name         TEXT        NOT NULL,
    -- scopes is reserved for Week 5 rate-limit classes / Week 7 billing
    -- attribution. JSONB DEFAULT '{}' lets callers add fields without
    -- a migration; current code ignores the payload.
    scopes       JSONB       NOT NULL DEFAULT '{}',
    revoked_at   TIMESTAMPTZ,
    created_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
    last_used_at TIMESTAMPTZ
);

-- Active-key lookups dominate the read path. A partial index over
-- (key_hash) WHERE revoked_at IS NULL keeps it small and fast.
CREATE INDEX idx_api_keys_active_hash
    ON api_keys (key_hash)
    WHERE revoked_at IS NULL;
