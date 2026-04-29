package embedding

import (
	"context"
	"errors"
	"fmt"

	lru "github.com/hashicorp/golang-lru/v2"
)

// LRUEmbedder is a decorator around any Embedder that memoizes
// results by input string. It's the answer to "same prompt asked
// many times pays the embed round-trip every time" — popular
// flatten texts hit the LRU and never reach the inner embedder.
//
// Safe for concurrent use; the underlying lru.Cache is goroutine-safe
// and the per-text classification loop holds no shared state.
//
// Storage cost: each cached entry is roughly Dimensions()*4 bytes for
// the vector plus the input string itself. text-embedding-3-small at
// 1536 dims = 6 KiB per entry; 1024 entries ≈ 6 MiB. Tune capacity
// against your prompt diversity, not request volume.
type LRUEmbedder struct {
	inner Embedder
	cache *lru.Cache[string, []float32]
}

// WithLRU wraps inner with an in-memory LRU keyed by input string.
// capacity is the number of distinct texts retained; ≤ 0 is invalid
// (use the inner embedder directly if you don't want caching).
func WithLRU(inner Embedder, capacity int) (*LRUEmbedder, error) {
	if inner == nil {
		return nil, errors.New("embedding: WithLRU requires non-nil inner Embedder")
	}
	if capacity <= 0 {
		return nil, fmt.Errorf("embedding: WithLRU capacity must be > 0, got %d", capacity)
	}
	c, err := lru.New[string, []float32](capacity)
	if err != nil {
		return nil, fmt.Errorf("embedding: build LRU: %w", err)
	}
	return &LRUEmbedder{inner: inner, cache: c}, nil
}

// Dimensions / Model are pass-throughs — the LRU is opaque to dim
// changes; if you swap inner to a different model you must build a
// fresh wrapper (mixing dims in the same LRU would silently corrupt
// downstream KNN results).
func (e *LRUEmbedder) Dimensions() int { return e.inner.Dimensions() }
func (e *LRUEmbedder) Model() string   { return e.inner.Model() }

// Len reports the current entry count. Exposed for metrics gauge in
// Week 10.7 + integration tests.
func (e *LRUEmbedder) Len() int { return e.cache.Len() }

// Embed handles partial hits: any subset of the inputs may already be
// cached. The inner embedder is only called when at least one input
// is a miss, and it's called exactly once with the missing texts in
// their original order, preserving batch efficiency. Results return
// in the same order as `texts`.
//
// Cached vectors are returned as defensive copies — callers that
// mutate the returned slice won't poison future LRU hits.
func (e *LRUEmbedder) Embed(ctx context.Context, texts []string) ([][]float32, error) {
	if len(texts) == 0 {
		return nil, ErrEmptyInput
	}

	out := make([][]float32, len(texts))
	missTexts := make([]string, 0, len(texts))
	missIdx := make([]int, 0, len(texts))

	for i, t := range texts {
		if v, ok := e.cache.Get(t); ok {
			out[i] = cloneVector(v)
			continue
		}
		missTexts = append(missTexts, t)
		missIdx = append(missIdx, i)
	}

	if len(missTexts) == 0 {
		return out, nil
	}

	fresh, err := e.inner.Embed(ctx, missTexts)
	if err != nil {
		return nil, err
	}
	if len(fresh) != len(missTexts) {
		return nil, fmt.Errorf("embedding: inner embedder returned %d vectors, expected %d",
			len(fresh), len(missTexts))
	}

	for j, vec := range fresh {
		// Store a defensive copy so a caller mutating their returned
		// slice can't change what the next hit sees.
		stored := cloneVector(vec)
		e.cache.Add(missTexts[j], stored)
		out[missIdx[j]] = cloneVector(stored)
	}
	return out, nil
}

func cloneVector(v []float32) []float32 {
	cp := make([]float32, len(v))
	copy(cp, v)
	return cp
}

// Compile-time check.
var _ Embedder = (*LRUEmbedder)(nil)
