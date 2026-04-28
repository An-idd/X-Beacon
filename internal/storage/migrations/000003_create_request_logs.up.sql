-- request_logs is the append-only ledger of every billed request. The
-- table is range-partitioned by `started_at` (1 partition / month) so
-- billing rollups can prune cheaply and old months can be detached for
-- cold storage without touching live writes.
--
-- Partitions are created lazily by internal/billing.EnsurePartition at
-- write time (idempotent CREATE TABLE IF NOT EXISTS). The migration
-- only declares the parent.
CREATE TABLE request_logs (
    id                BIGSERIAL,
    started_at        TIMESTAMPTZ NOT NULL,
    request_id        TEXT        NOT NULL,
    api_key_id        TEXT,
    provider          TEXT        NOT NULL,
    model             TEXT        NOT NULL,
    prompt_tokens     INTEGER     NOT NULL DEFAULT 0,
    completion_tokens INTEGER     NOT NULL DEFAULT 0,
    total_tokens      INTEGER     NOT NULL DEFAULT 0,
    latency_ms        INTEGER     NOT NULL DEFAULT 0,
    status            INTEGER     NOT NULL,
    streamed          BOOLEAN     NOT NULL DEFAULT FALSE,
    -- Cost breakdown in micro-units of `currency`. Pre-computed at write
    -- time so reports don't have to re-look-up pricing (which may have
    -- changed since the request).
    input_micro       BIGINT      NOT NULL DEFAULT 0,
    output_micro      BIGINT      NOT NULL DEFAULT 0,
    total_micro       BIGINT      NOT NULL DEFAULT 0,
    currency          TEXT        NOT NULL DEFAULT 'USD',
    error_code        TEXT,
    -- The PK is composite because partitioned tables require the
    -- partition key in every unique constraint; (id, started_at) is the
    -- canonical pattern.
    PRIMARY KEY (id, started_at)
) PARTITION BY RANGE (started_at);

-- Indexes are declared on the parent and propagate to every partition
-- created later. Two access patterns dominate:
--   - "All requests by api_key in <range>" → (api_key_id, started_at)
--   - "All requests for model in <range>"   → (model,     started_at)
CREATE INDEX idx_request_logs_api_key_time ON request_logs (api_key_id, started_at);
CREATE INDEX idx_request_logs_model_time   ON request_logs (model,      started_at);
