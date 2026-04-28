// Package billing accounts token usage and converts it to monetary cost.
// It owns:
//
//   - PricingCache  — in-memory model→rate map, loaded from model_pricing
//     and refreshed by admin writes / periodic reload.
//   - Worker (Step 7.3) — async writer that turns request events into
//     request_logs rows without blocking the chat hot path.
//   - Cost(...) helpers — pure-function conversion from (prompt_tokens,
//     completion_tokens, model) to micro-units of currency.
//
// Prices use BIGINT micro-units (1 USD = 1_000_000 micro-USD) so all
// math is exact integer arithmetic; conversion to float happens only at
// the API boundary.
package billing

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	"go.uber.org/zap"

	"github.com/An-idd/x-beacon/internal/observability"
	"github.com/An-idd/x-beacon/internal/storage"
)

// Rate captures the per-1k-token price for one model. Both fields are
// in micro-units (e.g. micro-USD). Zero values are legal and mean
// "untracked" — Cost() returns 0 so a missing row never blocks traffic.
type Rate struct {
	Model            string
	Currency         string
	InputPer1kMicro  int64
	OutputPer1kMicro int64
	UpdatedAt        time.Time
}

// PricingCache holds the active rate table in memory. Read paths use an
// atomic.Pointer to a snapshot map so reads are lock-free; writes
// (Reload, Set, Delete) atomically swap a fresh snapshot.
type PricingCache struct {
	pool    *storage.Pool
	metrics *observability.Metrics // nil-safe
	logger  *zap.Logger

	// snapshot is the read view. Updaters CAS-replace it; readers Load it.
	snapshot atomic.Pointer[map[string]Rate]

	// reloadMu serializes Reload/Set/Delete so concurrent admins don't
	// produce write-write torn snapshots. Reads are unaffected.
	reloadMu sync.Mutex
}

// SetMetrics attaches the gateway metrics bundle so cache writes
// publish a `gateway_pricing_cache_size` gauge update. Safe to call
// after construction; main wires it during startup once metrics are
// built. nil clears the binding (used by tests).
func (c *PricingCache) SetMetrics(m *observability.Metrics) {
	c.metrics = m
	// Emit current size so the gauge isn't stuck at 0 between startup
	// and the next reload tick.
	if snap := c.snapshot.Load(); snap != nil {
		c.metrics.SetPricingCacheSize(len(*snap))
	}
}

// NewPricingCache builds an empty cache pointing at pool. Callers must
// invoke Reload at startup before serving traffic; until Reload completes
// the cache returns 0-cost for every lookup, which is the documented
// "fail-open" behavior for untracked models.
func NewPricingCache(pool *storage.Pool, logger *zap.Logger) *PricingCache {
	c := &PricingCache{pool: pool, logger: logger}
	empty := make(map[string]Rate)
	c.snapshot.Store(&empty)
	return c
}

// Lookup returns the rate for model. The second return value is false
// when the model isn't priced; callers should still record usage but
// treat cost as 0.
func (c *PricingCache) Lookup(model string) (Rate, bool) {
	snap := c.snapshot.Load()
	if snap == nil {
		return Rate{}, false
	}
	r, ok := (*snap)[model]
	return r, ok
}

// All returns a copy of the current snapshot. Used by the admin GET
// listing handler; not on the hot path.
func (c *PricingCache) All() []Rate {
	snap := c.snapshot.Load()
	if snap == nil {
		return nil
	}
	out := make([]Rate, 0, len(*snap))
	for _, r := range *snap {
		out = append(out, r)
	}
	return out
}

// Reload replaces the snapshot with the latest model_pricing contents.
// Safe to call concurrently with Lookup; the swap is atomic.
func (c *PricingCache) Reload(ctx context.Context) error {
	if c.pool == nil {
		return errors.New("billing: cache has no pool; cannot reload")
	}
	c.reloadMu.Lock()
	defer c.reloadMu.Unlock()

	rows, err := c.pool.Query(ctx, `
		SELECT model, currency, input_per_1k_micro, output_per_1k_micro, updated_at
		  FROM model_pricing`)
	if err != nil {
		return fmt.Errorf("billing: query model_pricing: %w", err)
	}
	defer rows.Close()

	next := make(map[string]Rate)
	for rows.Next() {
		var r Rate
		if err := rows.Scan(&r.Model, &r.Currency, &r.InputPer1kMicro, &r.OutputPer1kMicro, &r.UpdatedAt); err != nil {
			return fmt.Errorf("billing: scan model_pricing: %w", err)
		}
		next[r.Model] = r
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("billing: rows model_pricing: %w", err)
	}

	c.snapshot.Store(&next)
	c.metrics.SetPricingCacheSize(len(next))
	if c.logger != nil {
		c.logger.Info("pricing cache reloaded", zap.Int("models", len(next)))
	}
	return nil
}

// Set upserts a single rate and refreshes the snapshot. Used by the
// admin PUT handler; the periodic reloader and Reload() take the bulk
// path instead.
func (c *PricingCache) Set(ctx context.Context, r Rate) error {
	if c.pool == nil {
		return errors.New("billing: cache has no pool; cannot set")
	}
	if r.Model == "" {
		return errors.New("billing: model is required")
	}
	if r.Currency == "" {
		r.Currency = "USD"
	}
	if r.InputPer1kMicro < 0 || r.OutputPer1kMicro < 0 {
		return errors.New("billing: rates must be >= 0")
	}

	c.reloadMu.Lock()
	defer c.reloadMu.Unlock()

	const q = `
		INSERT INTO model_pricing (model, currency, input_per_1k_micro, output_per_1k_micro)
		VALUES ($1, $2, $3, $4)
		ON CONFLICT (model) DO UPDATE
		   SET currency            = EXCLUDED.currency,
		       input_per_1k_micro  = EXCLUDED.input_per_1k_micro,
		       output_per_1k_micro = EXCLUDED.output_per_1k_micro,
		       updated_at          = now()
		RETURNING updated_at`
	if err := c.pool.QueryRow(ctx, q,
		r.Model, r.Currency, r.InputPer1kMicro, r.OutputPer1kMicro,
	).Scan(&r.UpdatedAt); err != nil {
		return fmt.Errorf("billing: upsert model_pricing: %w", err)
	}

	// Splice the row into a fresh snapshot. Cheaper than a full reload
	// for the common single-row update.
	c.replaceOne(r)
	return nil
}

// Delete removes one rate and refreshes the snapshot. Returns
// (false, nil) when the model wasn't priced — idempotent, no error.
func (c *PricingCache) Delete(ctx context.Context, model string) (bool, error) {
	if c.pool == nil {
		return false, errors.New("billing: cache has no pool; cannot delete")
	}
	c.reloadMu.Lock()
	defer c.reloadMu.Unlock()

	tag, err := c.pool.Exec(ctx, `DELETE FROM model_pricing WHERE model = $1`, model)
	if err != nil {
		return false, fmt.Errorf("billing: delete model_pricing: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return false, nil
	}
	c.removeOne(model)
	return true, nil
}

// replaceOne / removeOne are reloadMu-protected snapshot mutators. They
// build a fresh map (copy-on-write) so concurrent readers continue to
// observe the previous one until the atomic Store.
func (c *PricingCache) replaceOne(r Rate) {
	prev := c.snapshot.Load()
	next := make(map[string]Rate, len(*prev)+1)
	for k, v := range *prev {
		next[k] = v
	}
	next[r.Model] = r
	c.snapshot.Store(&next)
	c.metrics.SetPricingCacheSize(len(next))
}

func (c *PricingCache) removeOne(model string) {
	prev := c.snapshot.Load()
	if _, ok := (*prev)[model]; !ok {
		return
	}
	next := make(map[string]Rate, len(*prev))
	for k, v := range *prev {
		if k == model {
			continue
		}
		next[k] = v
	}
	c.snapshot.Store(&next)
	c.metrics.SetPricingCacheSize(len(next))
}

// Cost computes the total cost (in micro-units) of a single request
// given prompt + completion tokens and the model's rate. Both halves
// are charged independently; total is the sum.
//
// Returns (0, false) when the model isn't priced — caller may still
// log the row but should treat it as zero-cost (TODO list reconciles
// at end of period via the un-priced model count).
func Cost(rate Rate, promptTokens, completionTokens int) int64 {
	if rate.Model == "" {
		return 0
	}
	return int64(promptTokens)*rate.InputPer1kMicro/1000 +
		int64(completionTokens)*rate.OutputPer1kMicro/1000
}

// PeriodicReload starts a goroutine that calls Reload every interval.
// Cancel ctx to stop it. The first reload happens immediately so the
// cache is populated before traffic flows; subsequent ticks fire on
// the wall clock. Errors are logged but never propagated — a transient
// DB blip shouldn't kill the gateway.
func (c *PricingCache) PeriodicReload(ctx context.Context, interval time.Duration) {
	if interval <= 0 {
		return
	}
	go func() {
		// Eager first run to seed the cache.
		if err := c.Reload(ctx); err != nil && c.logger != nil {
			c.logger.Warn("initial pricing reload failed", zap.Error(err))
		}
		t := time.NewTicker(interval)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				if err := c.Reload(ctx); err != nil && c.logger != nil {
					c.logger.Warn("periodic pricing reload failed", zap.Error(err))
				}
			}
		}
	}()
}

