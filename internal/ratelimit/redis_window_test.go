package ratelimit

import (
	"context"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func newRedisHarness(t *testing.T) (*redis.Client, *miniredis.Miniredis) {
	t.Helper()
	mr := miniredis.RunT(t)
	client := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = client.Close() })
	return client, mr
}

func TestNewRedisWindow_RejectsBadConfig(t *testing.T) {
	client, _ := newRedisHarness(t)

	_, err := NewRedisWindow(RedisWindowConfig{})
	require.Error(t, err) // no client

	_, err = NewRedisWindow(RedisWindowConfig{Client: client, Limit: 0, Window: time.Second})
	require.Error(t, err)

	_, err = NewRedisWindow(RedisWindowConfig{Client: client, Limit: 5, Window: 0})
	require.Error(t, err)
}

func TestRedisWindow_AllowsUpToLimit(t *testing.T) {
	client, _ := newRedisHarness(t)
	rw, err := NewRedisWindow(RedisWindowConfig{Client: client, Limit: 3, Window: time.Second})
	require.NoError(t, err)

	for i := 0; i < 3; i++ {
		d, err := rw.Allow(context.Background(), "k1", 1)
		require.NoError(t, err)
		assert.True(t, d.Allowed, "request %d should pass", i)
		assert.Equal(t, 3, d.Limit)
	}
}

func TestRedisWindow_DeniesOverLimit(t *testing.T) {
	client, _ := newRedisHarness(t)
	rw, err := NewRedisWindow(RedisWindowConfig{Client: client, Limit: 2, Window: time.Second})
	require.NoError(t, err)

	_, _ = rw.Allow(context.Background(), "k1", 1)
	_, _ = rw.Allow(context.Background(), "k1", 1)

	d, err := rw.Allow(context.Background(), "k1", 1)
	require.NoError(t, err)
	assert.False(t, d.Allowed)
	assert.Equal(t, 2, d.Limit)
	assert.Equal(t, 0, d.Remaining)
	// RetryAfter must be positive and bounded by window.
	assert.Greater(t, d.RetryAfter, time.Duration(0))
	assert.LessOrEqual(t, d.RetryAfter, time.Second)
}

func TestRedisWindow_WindowSlide(t *testing.T) {
	// FastForward miniredis past the window: old entries drop, fresh
	// requests are allowed again.
	client, mr := newRedisHarness(t)
	rw, err := NewRedisWindow(RedisWindowConfig{Client: client, Limit: 2, Window: 200 * time.Millisecond})
	require.NoError(t, err)

	t0 := time.Date(2026, 4, 27, 12, 0, 0, 0, time.UTC)
	rw.now = func() time.Time { return t0 }

	_, _ = rw.Allow(context.Background(), "k1", 1)
	_, _ = rw.Allow(context.Background(), "k1", 1)
	d, _ := rw.Allow(context.Background(), "k1", 1)
	require.False(t, d.Allowed)

	// Slide past the window — but advance BOTH the clock the script sees
	// (rw.now) AND the miniredis clock (so the EXPIRE-driven cleanup
	// doesn't kick in independently mid-test).
	rw.now = func() time.Time { return t0.Add(300 * time.Millisecond) }
	mr.FastForward(300 * time.Millisecond)

	d, err = rw.Allow(context.Background(), "k1", 1)
	require.NoError(t, err)
	assert.True(t, d.Allowed, "after window slide, fresh request must pass")
}

func TestRedisWindow_DifferentKeysIndependent(t *testing.T) {
	client, _ := newRedisHarness(t)
	rw, err := NewRedisWindow(RedisWindowConfig{Client: client, Limit: 1, Window: time.Second})
	require.NoError(t, err)

	_, _ = rw.Allow(context.Background(), "k1", 1)
	d, err := rw.Allow(context.Background(), "k2", 1)
	require.NoError(t, err)
	assert.True(t, d.Allowed, "different key must be independent")
}

func TestRedisWindow_CostHigherThanLimit_Denies(t *testing.T) {
	client, _ := newRedisHarness(t)
	rw, err := NewRedisWindow(RedisWindowConfig{Client: client, Limit: 5, Window: time.Second})
	require.NoError(t, err)

	d, err := rw.Allow(context.Background(), "k1", 100)
	require.NoError(t, err)
	assert.False(t, d.Allowed)
}

func TestRedisWindow_KeyTTLApplied(t *testing.T) {
	// Belt-and-braces: after a successful Allow, the sorted-set key has
	// a TTL set (otherwise idle keys would leak forever).
	client, mr := newRedisHarness(t)
	rw, err := NewRedisWindow(RedisWindowConfig{Client: client, Limit: 5, Window: 2 * time.Second})
	require.NoError(t, err)

	_, err = rw.Allow(context.Background(), "k1", 1)
	require.NoError(t, err)

	// miniredis exposes per-key TTL; if not set it returns 0.
	assert.Greater(t, mr.TTL("k1"), time.Duration(0), "EXPIRE not applied")
}

func TestRedisWindow_BackendErrorPropagates(t *testing.T) {
	// Tear down miniredis to force a connection error.
	client, mr := newRedisHarness(t)
	rw, err := NewRedisWindow(RedisWindowConfig{Client: client, Limit: 5, Window: time.Second})
	require.NoError(t, err)
	mr.Close()

	_, err = rw.Allow(context.Background(), "k1", 1)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "redis script")
}

func TestRedisWindow_CostMultipleSlots(t *testing.T) {
	// cost=3 with limit=5 means three "slots" consumed in one call.
	// Two such calls should saturate the bucket; the third should deny.
	client, _ := newRedisHarness(t)
	rw, err := NewRedisWindow(RedisWindowConfig{Client: client, Limit: 5, Window: time.Second})
	require.NoError(t, err)

	d, _ := rw.Allow(context.Background(), "k1", 3)
	assert.True(t, d.Allowed)
	assert.Equal(t, 2, d.Remaining)

	d, _ = rw.Allow(context.Background(), "k1", 3)
	assert.False(t, d.Allowed, "5 - 3 = 2 < 3, second call must deny")
}
