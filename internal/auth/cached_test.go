package auth

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// fakeAuthn is a scriptable Authenticator for cache layer tests.
// Counts calls so tests can assert "inner was hit exactly once".
type fakeAuthn struct {
	mu        sync.Mutex
	calls     int32
	want      string
	principal *Principal
	failWith  error
	delay     time.Duration // simulated DB latency for singleflight tests
}

func (f *fakeAuthn) Authenticate(_ context.Context, key string) (*Principal, error) {
	atomic.AddInt32(&f.calls, 1)
	if f.delay > 0 {
		time.Sleep(f.delay)
	}
	if f.failWith != nil {
		return nil, f.failWith
	}
	if key == f.want {
		return f.principal, nil
	}
	return nil, ErrInvalidCredentials
}

func (f *fakeAuthn) Calls() int32 { return atomic.LoadInt32(&f.calls) }

// newRedisHarness spins up an embedded miniredis and a connected client.
// Cleanup is registered automatically.
func newRedisHarness(t *testing.T) (*redis.Client, *miniredis.Miniredis) {
	t.Helper()
	mr := miniredis.RunT(t)
	client := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = client.Close() })
	return client, mr
}

func TestCached_HitAvoidsInner(t *testing.T) {
	client, _ := newRedisHarness(t)
	inner := &fakeAuthn{want: "sk-good", principal: &Principal{ID: "k1", Name: "Test"}}
	c := NewCached(inner, client, time.Minute, 5*time.Second, nil)

	// First call: miss → inner hit, cache populated.
	p, err := c.Authenticate(context.Background(), "sk-good")
	require.NoError(t, err)
	require.NotNil(t, p)
	assert.Equal(t, "k1", p.ID)
	assert.EqualValues(t, 1, inner.Calls())

	// Second call: hit, inner unchanged.
	p2, err := c.Authenticate(context.Background(), "sk-good")
	require.NoError(t, err)
	assert.Equal(t, "k1", p2.ID)
	assert.EqualValues(t, 1, inner.Calls(), "inner must not be called on cache hit")
}

func TestCached_NegativeCache(t *testing.T) {
	client, _ := newRedisHarness(t)
	inner := &fakeAuthn{want: "sk-good", principal: &Principal{ID: "k1"}}
	c := NewCached(inner, client, time.Minute, 5*time.Second, nil)

	// Bad key: inner returns ErrInvalidCredentials, negative-cached.
	_, err := c.Authenticate(context.Background(), "sk-bad")
	require.Error(t, err)
	assert.True(t, errors.Is(err, ErrInvalidCredentials))
	assert.EqualValues(t, 1, inner.Calls())

	// Repeat: served from negative cache, inner not called.
	_, err = c.Authenticate(context.Background(), "sk-bad")
	require.Error(t, err)
	assert.True(t, errors.Is(err, ErrInvalidCredentials))
	assert.EqualValues(t, 1, inner.Calls(), "negative cache must short-circuit inner")
}

func TestCached_PositiveTTLExpiry(t *testing.T) {
	client, mr := newRedisHarness(t)
	inner := &fakeAuthn{want: "sk-good", principal: &Principal{ID: "k1"}}
	c := NewCached(inner, client, 10*time.Second, 5*time.Second, nil)

	_, err := c.Authenticate(context.Background(), "sk-good")
	require.NoError(t, err)
	assert.EqualValues(t, 1, inner.Calls())

	mr.FastForward(11 * time.Second)

	_, err = c.Authenticate(context.Background(), "sk-good")
	require.NoError(t, err)
	assert.EqualValues(t, 2, inner.Calls(), "TTL expiry must trigger another inner call")
}

func TestCached_NegativeTTLExpiry(t *testing.T) {
	client, mr := newRedisHarness(t)
	inner := &fakeAuthn{want: "sk-good", principal: &Principal{ID: "k1"}}
	c := NewCached(inner, client, time.Minute, 3*time.Second, nil)

	// First miss negative-caches.
	_, _ = c.Authenticate(context.Background(), "sk-bad")
	assert.EqualValues(t, 1, inner.Calls())

	// Within TTL: still cached.
	_, _ = c.Authenticate(context.Background(), "sk-bad")
	assert.EqualValues(t, 1, inner.Calls())

	// After TTL: re-asks inner. This is the "freshly issued key not
	// shadowed by stale not-found" guarantee.
	mr.FastForward(4 * time.Second)
	_, _ = c.Authenticate(context.Background(), "sk-bad")
	assert.EqualValues(t, 2, inner.Calls())
}

func TestCached_EmptyKeyShortCircuits(t *testing.T) {
	client, _ := newRedisHarness(t)
	inner := &fakeAuthn{}
	c := NewCached(inner, client, time.Minute, 5*time.Second, nil)

	_, err := c.Authenticate(context.Background(), "")
	require.Error(t, err)
	assert.True(t, errors.Is(err, ErrMissingCredentials))
	assert.EqualValues(t, 0, inner.Calls(), "empty key must not reach inner or Redis")
}

func TestCached_RedisOutageFailsOpen(t *testing.T) {
	client, mr := newRedisHarness(t)
	inner := &fakeAuthn{want: "sk-good", principal: &Principal{ID: "k1"}}
	c := NewCached(inner, client, time.Minute, 5*time.Second, nil)

	// Stop redis: every cache read errors. Auth should still succeed by
	// hitting inner directly.
	mr.Close()

	p, err := c.Authenticate(context.Background(), "sk-good")
	require.NoError(t, err)
	require.NotNil(t, p)
	assert.Equal(t, "k1", p.ID)
	assert.EqualValues(t, 1, inner.Calls())
}

func TestCached_BackendErrorNotCached(t *testing.T) {
	client, _ := newRedisHarness(t)
	inner := &fakeAuthn{failWith: errors.New("db: connection refused")}
	c := NewCached(inner, client, time.Minute, 5*time.Second, nil)

	_, err := c.Authenticate(context.Background(), "sk-anything")
	require.Error(t, err)
	assert.EqualValues(t, 1, inner.Calls())

	// Again — must hit inner again because transient errors are not cached.
	_, err = c.Authenticate(context.Background(), "sk-anything")
	require.Error(t, err)
	assert.EqualValues(t, 2, inner.Calls(),
		"backend errors must NOT poison the cache; transient outages should retry")
}

func TestCached_Singleflight_CoalescesConcurrentMisses(t *testing.T) {
	// 50 goroutines hammering the same key during a slow inner call.
	// With singleflight, inner should be called exactly once.
	client, _ := newRedisHarness(t)
	inner := &fakeAuthn{
		want:      "sk-good",
		principal: &Principal{ID: "k1"},
		delay:     50 * time.Millisecond,
	}
	c := NewCached(inner, client, time.Minute, 5*time.Second, nil)

	const N = 50
	var wg sync.WaitGroup
	wg.Add(N)
	results := make([]error, N)
	for i := 0; i < N; i++ {
		go func(i int) {
			defer wg.Done()
			_, results[i] = c.Authenticate(context.Background(), "sk-good")
		}(i)
	}
	wg.Wait()

	for i, e := range results {
		assert.NoError(t, e, "goroutine %d", i)
	}
	assert.EqualValues(t, 1, inner.Calls(), "singleflight must coalesce N concurrent misses to 1")
}

func TestCached_NilInnerErrors(t *testing.T) {
	client, _ := newRedisHarness(t)
	c := NewCached(nil, client, time.Minute, 5*time.Second, nil)

	_, err := c.Authenticate(context.Background(), "sk-x")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "no inner")
}

func TestCached_MalformedCacheValueFallsThrough(t *testing.T) {
	client, mr := newRedisHarness(t)
	inner := &fakeAuthn{want: "sk-good", principal: &Principal{ID: "k1"}}
	c := NewCached(inner, client, time.Minute, 5*time.Second, nil)

	cacheKey := "auth:k:" + hashKeyHex("sk-good")
	require.NoError(t, mr.Set(cacheKey, "{not valid json"))

	p, err := c.Authenticate(context.Background(), "sk-good")
	require.NoError(t, err)
	require.NotNil(t, p)
	assert.Equal(t, "k1", p.ID)
	assert.EqualValues(t, 1, inner.Calls(), "garbage cache value must trigger inner fallback")
}

func TestCached_DisabledTTLs(t *testing.T) {
	// posTTL=0, negTTL=0: cache reads still happen (in case Redis was
	// pre-populated by an admin tool) but writes are skipped. Useful for
	// testing the read path in isolation.
	client, _ := newRedisHarness(t)
	inner := &fakeAuthn{want: "sk-good", principal: &Principal{ID: "k1"}}
	c := NewCached(inner, client, 0, 0, nil)

	_, _ = c.Authenticate(context.Background(), "sk-good")
	_, _ = c.Authenticate(context.Background(), "sk-good")
	assert.EqualValues(t, 2, inner.Calls(),
		"with posTTL=0, no caching, every call hits inner")
}
