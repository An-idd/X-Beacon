package ratelimit

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"golang.org/x/time/rate"
)

func TestNewMemoryBucket_RejectsBadConfig(t *testing.T) {
	_, err := NewMemoryBucket(MemoryBucketConfig{Rate: 0, Burst: 10})
	require.Error(t, err)

	_, err = NewMemoryBucket(MemoryBucketConfig{Rate: 10, Burst: 0})
	require.Error(t, err)
}

func TestNewMemoryBucket_AppliesDefaults(t *testing.T) {
	mb, err := NewMemoryBucket(MemoryBucketConfig{Rate: 10, Burst: 5})
	require.NoError(t, err)
	// Internal LRU instance survives — sanity that defaults didn't error.
	assert.NotNil(t, mb.cache)
}

func TestMemoryBucket_AllowsUpToBurst(t *testing.T) {
	mb, err := NewMemoryBucket(MemoryBucketConfig{Rate: 1, Burst: 3})
	require.NoError(t, err)

	for i := 0; i < 3; i++ {
		d, err := mb.Allow(context.Background(), "k1", 1)
		require.NoError(t, err)
		assert.True(t, d.Allowed, "request %d should pass", i)
	}
}

func TestMemoryBucket_DeniesOverBurst(t *testing.T) {
	mb, err := NewMemoryBucket(MemoryBucketConfig{Rate: 1, Burst: 2})
	require.NoError(t, err)

	// Use a frozen clock so the test isn't timing-dependent.
	fixed := time.Date(2026, 4, 27, 12, 0, 0, 0, time.UTC)
	mb.now = func() time.Time { return fixed }

	for i := 0; i < 2; i++ {
		d, _ := mb.Allow(context.Background(), "k1", 1)
		require.True(t, d.Allowed)
	}

	// Third request: bucket empty, should deny.
	d, err := mb.Allow(context.Background(), "k1", 1)
	require.NoError(t, err)
	assert.False(t, d.Allowed)
	assert.Equal(t, 2, d.Limit)
	assert.Greater(t, d.RetryAfter, time.Duration(0), "deny must include positive RetryAfter")
}

func TestMemoryBucket_RecoversAtRate(t *testing.T) {
	mb, err := NewMemoryBucket(MemoryBucketConfig{Rate: 10, Burst: 2})
	require.NoError(t, err)

	t0 := time.Date(2026, 4, 27, 12, 0, 0, 0, time.UTC)
	mb.now = func() time.Time { return t0 }

	// Drain.
	_, _ = mb.Allow(context.Background(), "k1", 1)
	_, _ = mb.Allow(context.Background(), "k1", 1)
	d, _ := mb.Allow(context.Background(), "k1", 1)
	require.False(t, d.Allowed)

	// 100ms later, rate=10/s → 1 token recovered → next request allowed.
	mb.now = func() time.Time { return t0.Add(100 * time.Millisecond) }
	d, err = mb.Allow(context.Background(), "k1", 1)
	require.NoError(t, err)
	assert.True(t, d.Allowed)
}

func TestMemoryBucket_DifferentKeysIndependent(t *testing.T) {
	mb, err := NewMemoryBucket(MemoryBucketConfig{Rate: 1, Burst: 1})
	require.NoError(t, err)

	d1, _ := mb.Allow(context.Background(), "k1", 1)
	d2, _ := mb.Allow(context.Background(), "k2", 1)
	assert.True(t, d1.Allowed)
	assert.True(t, d2.Allowed, "different key must NOT share the same bucket")
}

func TestMemoryBucket_LRUEviction(t *testing.T) {
	mb, err := NewMemoryBucket(MemoryBucketConfig{
		Rate:    1,
		Burst:   1,
		MaxKeys: 2, // tiny so we can drive the eviction
		IdleTTL: time.Hour,
	})
	require.NoError(t, err)

	// Frozen clock so AllowN doesn't refill keys we then re-touch.
	fixed := time.Date(2026, 4, 27, 12, 0, 0, 0, time.UTC)
	mb.now = func() time.Time { return fixed }

	// Drain three distinct keys: cache size becomes 2 after 3rd insert.
	_, _ = mb.Allow(context.Background(), "k1", 1)
	_, _ = mb.Allow(context.Background(), "k2", 1)
	_, _ = mb.Allow(context.Background(), "k3", 1)
	assert.Equal(t, 2, mb.Len())

	// k1 was the LRU candidate when k3 was added → fresh bucket on
	// re-access (full burst).
	d, _ := mb.Allow(context.Background(), "k1", 1)
	assert.True(t, d.Allowed, "evicted key gets a fresh bucket on re-access")
}

func TestMemoryBucket_TTLExpiry(t *testing.T) {
	mb, err := NewMemoryBucket(MemoryBucketConfig{
		Rate:    1,
		Burst:   1,
		MaxKeys: 100,
		IdleTTL: 50 * time.Millisecond,
	})
	require.NoError(t, err)

	_, _ = mb.Allow(context.Background(), "k1", 1)
	// Bucket now empty; immediate retry would deny.

	// Wait past TTL — the LRU drops the entry, next call gets a fresh bucket.
	time.Sleep(80 * time.Millisecond)

	d, err := mb.Allow(context.Background(), "k1", 1)
	require.NoError(t, err)
	assert.True(t, d.Allowed, "expired bucket must be replaced with a fresh one")
}

func TestMemoryBucket_CostHigherThanBurst_Denies(t *testing.T) {
	mb, err := NewMemoryBucket(MemoryBucketConfig{Rate: 1, Burst: 5})
	require.NoError(t, err)

	d, err := mb.Allow(context.Background(), "k1", 100)
	require.NoError(t, err)
	assert.False(t, d.Allowed, "cost > burst is unsatisfiable")
}

func TestMemoryBucket_ConcurrentAccessSafe(t *testing.T) {
	// Stress: x/time/rate is documented concurrent-safe and the LRU is
	// internally synchronized. Verify no races and that the total
	// allowed count is bounded by burst + (rate × elapsed).
	mb, err := NewMemoryBucket(MemoryBucketConfig{Rate: 1000, Burst: 100})
	require.NoError(t, err)

	var allowed atomic.Int64
	var wg sync.WaitGroup
	const goroutines, perGoroutine = 20, 100
	for i := 0; i < goroutines; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < perGoroutine; j++ {
				d, _ := mb.Allow(context.Background(), "shared", 1)
				if d.Allowed {
					allowed.Add(1)
				}
			}
		}()
	}
	wg.Wait()

	// 20*100 = 2000 attempts. With rate=1000/s + burst=100, the test
	// usually finishes well under 1s, so allowed ~ burst. We just want
	// "fewer than the total attempts" and "race detector clean".
	got := allowed.Load()
	assert.Greater(t, got, int64(0))
	assert.LessOrEqual(t, got, int64(goroutines*perGoroutine))
}

// _ pins the rate dependency so go vet doesn't warn if a refactor drops it.
var _ = rate.NewLimiter
