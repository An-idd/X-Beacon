-- model_pricing is the single source of truth for billing rates. One row
-- per model id; in-memory cache (internal/billing.PricingCache) reloads
-- from this table at startup, on admin write, and on a periodic timer.
--
-- Prices are quoted per 1000 tokens (industry convention) in micro-units
-- of the configured currency to avoid float drift in billing math.
-- 1 USD = 1_000_000 micro-USD; e.g. GPT-4o input = $5.00/1M tokens =
-- $0.005/1k = 5000 micro-USD.
CREATE TABLE model_pricing (
    model               TEXT        PRIMARY KEY,
    -- Currency is held alongside the rate so a future multi-currency
    -- billing run doesn't require a migration. Default USD covers all
    -- current upstreams.
    currency            TEXT        NOT NULL DEFAULT 'USD',
    -- Cost in micro-units per 1000 tokens. BIGINT keeps integer math
    -- safe up to absurd quantities; INTEGER would overflow at ~2 billion
    -- micro-units = $2k per 1k tokens, which is fine but BIGINT is free.
    input_per_1k_micro  BIGINT      NOT NULL CHECK (input_per_1k_micro  >= 0),
    output_per_1k_micro BIGINT      NOT NULL CHECK (output_per_1k_micro >= 0),
    -- updated_at lets the periodic reloader (and admin UI) show which
    -- rows changed without diffing the whole table. Bumped on every UPSERT.
    updated_at          TIMESTAMPTZ NOT NULL DEFAULT now(),
    created_at          TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- Seed the catalogue with current public pricing so a fresh deploy can
-- bill out of the box. Operators override via xbctl / admin API later.
-- Numbers are 2025-Q4 list prices; absent entries fall back to "free"
-- (see billing.PricingCache.Lookup) so a missing row never blocks
-- traffic — it just yields zero-cost rows in request_logs.
INSERT INTO model_pricing (model, input_per_1k_micro, output_per_1k_micro) VALUES
    ('gpt-4o',                5000, 15000),
    ('gpt-4o-mini',            150,   600),
    ('gpt-4-turbo',          10000, 30000),
    ('gpt-3.5-turbo',          500,  1500),
    ('claude-3-5-sonnet',     3000, 15000),
    ('claude-3-5-sonnet-20241022', 3000, 15000),
    ('claude-3-opus',        15000, 75000),
    ('claude-3-haiku',         250,  1250),
    ('deepseek-chat',          140,   280),
    ('deepseek-reasoner',      550,  2190);
