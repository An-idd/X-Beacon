-- admin_audit_logs records every operator action that mutates the
-- admin surface (key create/revoke, pricing upsert/delete, etc.).
-- Read-only after write. The point is forensic accountability —
-- "who changed pricing for gpt-4o on Saturday at 3am?"
--
-- Synchronous insert from the admin handlers; volume is tiny
-- (operator clicks per day, not per second), so partitioning is
-- overkill — single un-partitioned table indexed for the two
-- access patterns we expect:
--   - "show me everything that happened in <range>"
--   - "show me what <actor> did"
CREATE TABLE admin_audit_logs (
    id            BIGSERIAL PRIMARY KEY,
    occurred_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
    actor_id      TEXT        NOT NULL,
    -- actor_label is a snapshot of api_keys.name at the moment of
    -- the action. Stored separately so a later rename / revoke
    -- doesn't rewrite history.
    actor_label   TEXT        NOT NULL,
    -- action format: <category>.<verb>; e.g. key.create, key.revoke,
    -- pricing.upsert, pricing.delete. The CHECK enforces a dot so
    -- callers don't drift into unstructured strings.
    action        TEXT        NOT NULL CHECK (action LIKE '%.%'),
    target_type   TEXT        NOT NULL,
    target_id     TEXT        NOT NULL,
    -- metadata is action-specific JSON. Examples:
    --   key.create   → {"label": "...", "scopes": [...]}
    --   pricing.upsert → {"input_per_1k": 0.005, "output_per_1k": 0.015}
    -- Never holds prompts / response content (separate concern).
    metadata      JSONB,
    request_id    TEXT
);

CREATE INDEX idx_admin_audit_logs_time
    ON admin_audit_logs (occurred_at DESC);

CREATE INDEX idx_admin_audit_logs_actor_time
    ON admin_audit_logs (actor_id, occurred_at DESC);
