package billing

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	"github.com/An-idd/x-beacon/internal/storage"
)

func TestCost_NoModelReturnsZero(t *testing.T) {
	// A zero-value Rate (no model row) → free, never errors.
	assert.Equal(t, int64(0), Cost(Rate{}, 100, 200))
}

func TestCost_PerThousandTokens(t *testing.T) {
	// $0.005 per 1k input = 5000 micro-USD per 1k.
	r := Rate{Model: "gpt-4o", Currency: "USD", InputPer1kMicro: 5000, OutputPer1kMicro: 15000}

	// Exactly 1000 prompt tokens → 5000 micro.
	assert.Equal(t, int64(5000), Cost(r, 1000, 0))
	// Exactly 1000 completion tokens → 15000 micro.
	assert.Equal(t, int64(15000), Cost(r, 0, 1000))
	// Mixed.
	assert.Equal(t, int64(5000+15000), Cost(r, 1000, 1000))
}

func TestCost_RoundsTowardZero(t *testing.T) {
	// Integer division truncates after the / 1000 step. For typical
	// rates this rounding favors the user fractionally and avoids
	// float drift.
	r := Rate{Model: "x", InputPer1kMicro: 5000} // $0.005/1k
	assert.Equal(t, int64(495), Cost(r, 99, 0))  // 99 × 5000 / 1000 = 495
	assert.Equal(t, int64(5), Cost(r, 1, 0))     // 1  × 5000 / 1000 = 5

	// At a much cheaper rate the truncation bites: 1 token × 1/1000 = 0.
	r2 := Rate{Model: "y", InputPer1kMicro: 1}
	assert.Equal(t, int64(0), Cost(r2, 1, 0))
	assert.Equal(t, int64(1), Cost(r2, 1000, 0)) // exactly 1
}

func TestPricingCache_LookupMissReturnsFalse(t *testing.T) {
	c := NewPricingCache(nil, zap.NewNop())
	r, ok := c.Lookup("nonexistent")
	assert.False(t, ok)
	assert.Equal(t, Rate{}, r)
}

func TestPricingCache_AllEmptyOnInit(t *testing.T) {
	c := NewPricingCache(nil, zap.NewNop())
	assert.Empty(t, c.All())
}

func TestPricingCache_ReplaceOneAndRemoveOne(t *testing.T) {
	// White-box test of the snapshot mutators (used by Set/Delete to
	// avoid full Reload on single-row changes).
	c := NewPricingCache(nil, zap.NewNop())

	c.replaceOne(Rate{Model: "a", InputPer1kMicro: 1, OutputPer1kMicro: 2})
	c.replaceOne(Rate{Model: "b", InputPer1kMicro: 3, OutputPer1kMicro: 4})
	assert.Len(t, c.All(), 2)

	got, ok := c.Lookup("a")
	require.True(t, ok)
	assert.Equal(t, int64(1), got.InputPer1kMicro)

	// Updating "a" must not duplicate the entry.
	c.replaceOne(Rate{Model: "a", InputPer1kMicro: 99})
	assert.Len(t, c.All(), 2)
	got, _ = c.Lookup("a")
	assert.Equal(t, int64(99), got.InputPer1kMicro)

	c.removeOne("a")
	_, ok = c.Lookup("a")
	assert.False(t, ok)
	assert.Len(t, c.All(), 1)

	// Removing a non-existent key is a no-op.
	c.removeOne("zzz")
	assert.Len(t, c.All(), 1)
}

func TestPricingCache_ReloadWithoutPoolErrs(t *testing.T) {
	c := NewPricingCache(nil, zap.NewNop())
	err := c.Reload(context.Background())
	require.Error(t, err)
}

func TestPricingCache_SetValidations(t *testing.T) {
	c := NewPricingCache(nil, zap.NewNop())
	// Empty model — caught before the pool nil-check below.
	require.Error(t, c.Set(context.Background(), Rate{Model: ""}))
	// Negative rate — same.
	require.Error(t, c.Set(context.Background(), Rate{Model: "x", InputPer1kMicro: -1}))
}

// --- DB-gated integration tests ---

func integrationPool(t *testing.T) *storage.Pool {
	t.Helper()
	dsn := os.Getenv("XBEACON_TEST_DSN")
	if dsn == "" {
		t.Skip("set XBEACON_TEST_DSN to run integration tests")
	}
	require.NoError(t, storage.MigrateDown(dsn))
	require.NoError(t, storage.MigrateUp(dsn))
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	pool, err := storage.NewPool(ctx, storage.Config{DSN: dsn})
	require.NoError(t, err)
	t.Cleanup(pool.Close)
	return pool
}

func TestPricingCache_ReloadFromDB(t *testing.T) {
	pool := integrationPool(t)
	c := NewPricingCache(pool, zap.NewNop())

	require.NoError(t, c.Reload(context.Background()))

	// Migration seeds 10 rows; assert non-empty and one known model.
	assert.GreaterOrEqual(t, len(c.All()), 10)
	r, ok := c.Lookup("gpt-4o")
	require.True(t, ok)
	assert.Equal(t, int64(5000), r.InputPer1kMicro)
}

func TestPricingCache_SetThenLookup(t *testing.T) {
	pool := integrationPool(t)
	c := NewPricingCache(pool, zap.NewNop())
	require.NoError(t, c.Reload(context.Background()))

	require.NoError(t, c.Set(context.Background(), Rate{
		Model: "test-pricing-model", Currency: "USD",
		InputPer1kMicro: 10, OutputPer1kMicro: 20,
	}))
	r, ok := c.Lookup("test-pricing-model")
	require.True(t, ok)
	assert.Equal(t, int64(10), r.InputPer1kMicro)
	assert.Equal(t, "USD", r.Currency)
	assert.False(t, r.UpdatedAt.IsZero())

	// Update existing row — UpdatedAt advances.
	prev := r.UpdatedAt
	time.Sleep(10 * time.Millisecond)
	require.NoError(t, c.Set(context.Background(), Rate{
		Model: "test-pricing-model", InputPer1kMicro: 11, OutputPer1kMicro: 21,
	}))
	r, _ = c.Lookup("test-pricing-model")
	assert.Equal(t, int64(11), r.InputPer1kMicro)
	assert.True(t, r.UpdatedAt.After(prev))
}

func TestPricingCache_DeleteIdempotent(t *testing.T) {
	pool := integrationPool(t)
	c := NewPricingCache(pool, zap.NewNop())
	require.NoError(t, c.Reload(context.Background()))

	// First delete on a seeded row → true.
	deleted, err := c.Delete(context.Background(), "gpt-4o")
	require.NoError(t, err)
	assert.True(t, deleted)
	_, ok := c.Lookup("gpt-4o")
	assert.False(t, ok)

	// Second delete → false (idempotent), no error.
	deleted, err = c.Delete(context.Background(), "gpt-4o")
	require.NoError(t, err)
	assert.False(t, deleted)
}

func TestPricingCache_PeriodicReload(t *testing.T) {
	pool := integrationPool(t)
	c := NewPricingCache(pool, zap.NewNop())

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	c.PeriodicReload(ctx, 50*time.Millisecond)

	// Wait for first reload to land. The eager initial run completes
	// before the ticker fires.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if len(c.All()) > 0 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	assert.NotEmpty(t, c.All())
}
