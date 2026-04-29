package embedding

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// recordingEmbedder is a minimal Embedder that records every input
// passed to Embed and returns deterministic vectors keyed by the
// input string. Used to verify the LRU wrapper's cache-hit / -miss
// classification.
type recordingEmbedder struct {
	dim     int
	calls   atomic.Int64
	lastIn  []string
	mu      sync.Mutex
	err     error
}

func (r *recordingEmbedder) Embed(_ context.Context, texts []string) ([][]float32, error) {
	r.calls.Add(1)
	r.mu.Lock()
	r.lastIn = append([]string(nil), texts...)
	r.mu.Unlock()
	if r.err != nil {
		return nil, r.err
	}
	out := make([][]float32, len(texts))
	for i, t := range texts {
		// Vec is "first byte of t in slot 0, then zeros". Distinct per
		// distinct first character, sufficient for the assertions here.
		v := make([]float32, r.dim)
		if len(t) > 0 {
			v[0] = float32(t[0])
		}
		out[i] = v
	}
	return out, nil
}
func (r *recordingEmbedder) Dimensions() int { return r.dim }
func (r *recordingEmbedder) Model() string   { return "recording" }

func newWrapped(t *testing.T, capacity int) (*LRUEmbedder, *recordingEmbedder) {
	t.Helper()
	inner := &recordingEmbedder{dim: 4}
	wrapped, err := WithLRU(inner, capacity)
	require.NoError(t, err)
	return wrapped, inner
}

func TestWithLRU_Validation(t *testing.T) {
	_, err := WithLRU(nil, 8)
	require.Error(t, err)

	inner := &recordingEmbedder{dim: 4}
	_, err = WithLRU(inner, 0)
	require.Error(t, err)
	_, err = WithLRU(inner, -1)
	require.Error(t, err)
}

func TestLRUEmbedder_PassesDimensionsAndModel(t *testing.T) {
	wrapped, _ := newWrapped(t, 4)
	assert.Equal(t, 4, wrapped.Dimensions())
	assert.Equal(t, "recording", wrapped.Model())
}

func TestLRUEmbedder_FirstCallIsAlwaysMiss(t *testing.T) {
	wrapped, inner := newWrapped(t, 8)
	vecs, err := wrapped.Embed(context.Background(), []string{"hello"})
	require.NoError(t, err)
	require.Len(t, vecs, 1)
	assert.Equal(t, int64(1), inner.calls.Load(), "cold call must reach inner")
	assert.Equal(t, []string{"hello"}, inner.lastIn)
	assert.Equal(t, 1, wrapped.Len())
}

func TestLRUEmbedder_RepeatedCallSkipsInner(t *testing.T) {
	wrapped, inner := newWrapped(t, 8)
	_, _ = wrapped.Embed(context.Background(), []string{"hello"})
	require.Equal(t, int64(1), inner.calls.Load())

	for i := 0; i < 5; i++ {
		_, err := wrapped.Embed(context.Background(), []string{"hello"})
		require.NoError(t, err)
	}
	assert.Equal(t, int64(1), inner.calls.Load(),
		"repeated identical inputs must not reach inner")
}

func TestLRUEmbedder_PartialHitOnlyFetchesMissingTexts(t *testing.T) {
	wrapped, inner := newWrapped(t, 8)

	// Warm with two texts.
	_, err := wrapped.Embed(context.Background(), []string{"alpha", "beta"})
	require.NoError(t, err)
	require.Equal(t, int64(1), inner.calls.Load())

	// Now ask for [alpha, gamma, beta]. Only "gamma" should hit inner.
	vecs, err := wrapped.Embed(context.Background(), []string{"alpha", "gamma", "beta"})
	require.NoError(t, err)
	require.Len(t, vecs, 3)
	assert.Equal(t, int64(2), inner.calls.Load())
	assert.Equal(t, []string{"gamma"}, inner.lastIn,
		"only the missing text must be sent to the inner embedder")

	// Returned vectors must reflect each input's distinct first byte —
	// proves order is preserved across the partial-hit shuffle.
	assert.Equal(t, float32('a'), vecs[0][0])
	assert.Equal(t, float32('g'), vecs[1][0])
	assert.Equal(t, float32('b'), vecs[2][0])
}

func TestLRUEmbedder_AllHitNoInnerCall(t *testing.T) {
	wrapped, inner := newWrapped(t, 8)
	_, err := wrapped.Embed(context.Background(), []string{"alpha", "beta"})
	require.NoError(t, err)
	require.Equal(t, int64(1), inner.calls.Load())

	_, err = wrapped.Embed(context.Background(), []string{"alpha", "beta"})
	require.NoError(t, err)
	assert.Equal(t, int64(1), inner.calls.Load(),
		"fully-cached batch must not reach inner")
}

func TestLRUEmbedder_DefensiveCopy(t *testing.T) {
	wrapped, _ := newWrapped(t, 8)
	first, err := wrapped.Embed(context.Background(), []string{"key"})
	require.NoError(t, err)

	// Mutate the caller's view; subsequent hit must NOT see the change.
	first[0][0] = 999

	second, err := wrapped.Embed(context.Background(), []string{"key"})
	require.NoError(t, err)
	assert.NotEqual(t, float32(999), second[0][0],
		"cached vector must be isolated from caller mutation")
}

func TestLRUEmbedder_EmptyInput(t *testing.T) {
	wrapped, inner := newWrapped(t, 8)
	_, err := wrapped.Embed(context.Background(), nil)
	assert.True(t, errors.Is(err, ErrEmptyInput))
	assert.Equal(t, int64(0), inner.calls.Load())

	_, err = wrapped.Embed(context.Background(), []string{})
	assert.True(t, errors.Is(err, ErrEmptyInput))
}

func TestLRUEmbedder_InnerErrorPropagates(t *testing.T) {
	inner := &recordingEmbedder{dim: 4, err: errors.New("upstream down")}
	wrapped, err := WithLRU(inner, 8)
	require.NoError(t, err)

	_, err = wrapped.Embed(context.Background(), []string{"x"})
	require.Error(t, err)
	assert.Equal(t, 0, wrapped.Len(), "errored fetch must not pollute the cache")
}

func TestLRUEmbedder_EvictionAtCapacity(t *testing.T) {
	wrapped, inner := newWrapped(t, 2)

	_, _ = wrapped.Embed(context.Background(), []string{"a"})
	_, _ = wrapped.Embed(context.Background(), []string{"b"})
	require.Equal(t, int64(2), inner.calls.Load())
	require.Equal(t, 2, wrapped.Len())

	// Insert a 3rd; oldest ("a") evicted.
	_, _ = wrapped.Embed(context.Background(), []string{"c"})
	require.Equal(t, int64(3), inner.calls.Load())
	assert.Equal(t, 2, wrapped.Len())

	// "a" is evicted; re-fetching it must hit inner again.
	_, _ = wrapped.Embed(context.Background(), []string{"a"})
	assert.Equal(t, int64(4), inner.calls.Load())
}

func TestLRUEmbedder_ConcurrentSafe(t *testing.T) {
	wrapped, _ := newWrapped(t, 64)
	const goroutines = 16
	const reps = 50

	var wg sync.WaitGroup
	wg.Add(goroutines)
	for g := 0; g < goroutines; g++ {
		go func(g int) {
			defer wg.Done()
			for i := 0; i < reps; i++ {
				keys := []string{
					"shared-1", "shared-2",
					"per-goroutine-" + string(rune('a'+g)),
				}
				_, err := wrapped.Embed(context.Background(), keys)
				require.NoError(t, err)
			}
		}(g)
	}
	wg.Wait()
}
