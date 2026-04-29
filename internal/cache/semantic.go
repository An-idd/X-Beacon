package cache

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/An-idd/x-beacon/internal/provider"
	"github.com/An-idd/x-beacon/pkg/embedding"
)

// Semantic is the lookup-and-insert contract chat handlers consume
// when an exact-match miss reaches the semantic layer.
//
// The pipeline is:
//
//	Lookup:  flatten(req) → embed → KNN(top-K) → threshold filter
//	Insert:  flatten(req) → embed → SemanticIndex.Insert(payload=resp JSON)
//
// Both Lookup and Insert are best-effort: the chat hot path treats
// any error other than ErrMiss as a transient miss and keeps moving.
//
// Multi-model isolation is the assembly's job. A single SemanticCache
// instance shares one SemanticIndex; deployments serving multiple
// models should construct one cache per model with a per-model
// IndexName (Decision 2). A Selector wrapper for that pattern lives
// in cmd/gateway.
type Semantic interface {
	// Lookup returns the cached response when the nearest neighbor's
	// cosine similarity meets the configured threshold. The float64 is
	// that similarity (0..1) — useful for the caller to log/observe
	// regardless of hit/miss. Returns ErrMiss when no neighbor passes
	// the threshold, when flatten produces an empty string, or when
	// the index is empty.
	Lookup(ctx context.Context, req *provider.ChatRequest) (*provider.ChatResponse, float64, error)

	// Insert stores resp under the request's flattened text, computing
	// the embedding inline. ChatResponse is serialized as JSON and
	// stuffed into the SemanticIndex payload field so a future hit
	// can return immediately without a second round-trip.
	Insert(ctx context.Context, req *provider.ChatRequest, resp *provider.ChatResponse) error
}

// SemanticConfig parameterizes NewSemanticCache. Threshold uses
// cosine *similarity* semantics (1.0 = identical) per Decision 5;
// the conversion from RediSearch's cosine *distance* happens
// internally so config files and dashboards can use the natural form.
type SemanticConfig struct {
	// Embedder produces query/document vectors. Required.
	Embedder embedding.Embedder

	// Index is the vector store. Required. Dimensions must match
	// Embedder.Dimensions() — checked at construction.
	Index SemanticIndex

	// Threshold is the minimum cosine similarity that counts as a hit
	// (0..1). 0.95 is the configured default elsewhere; values below
	// 0.7 risk frequent false-positive hits.
	Threshold float64

	// TopK caps the candidates returned by the index. Default 5; the
	// best-match-first contract makes K=1 sufficient for hit/miss but
	// K>1 is forward-compatible with re-ranking experiments.
	TopK int
}

// SemanticCache implements Semantic with a single Embedder + a single
// SemanticIndex. Safe for concurrent use after construction.
type SemanticCache struct {
	cfg            SemanticConfig
	cosineCutoff   float64 // pre-computed: distance <= cosineCutoff means hit
	maxSimilarity  float64 // 1.0 — kept as a named constant for readability
}

// NewSemanticCache validates cfg and returns the cache. Embedder
// dimension must match Index dimension; an early panic at startup
// beats a silent dimension-mismatch error per request.
//
// Threshold is rejected outside [0, 1]: cosine similarity is bounded
// and a value outside the range almost certainly means the operator
// confused similarity with distance.
func NewSemanticCache(cfg SemanticConfig) (*SemanticCache, error) {
	if cfg.Embedder == nil {
		return nil, errors.New("cache: SemanticConfig.Embedder is required")
	}
	if cfg.Index == nil {
		return nil, errors.New("cache: SemanticConfig.Index is required")
	}
	if cfg.Threshold < 0 || cfg.Threshold > 1 {
		return nil, fmt.Errorf("cache: SemanticConfig.Threshold must be in [0,1], got %v", cfg.Threshold)
	}
	if cfg.TopK <= 0 {
		cfg.TopK = 5
	}
	if redisIdx, ok := cfg.Index.(*RedisSemanticIndex); ok {
		if redisIdx.cfg.Dimensions != cfg.Embedder.Dimensions() {
			return nil, fmt.Errorf("cache: dimension mismatch — Embedder=%d Index=%d",
				cfg.Embedder.Dimensions(), redisIdx.cfg.Dimensions)
		}
	}
	// RediSearch cosine distance ranges over [0, 2]. similarity = 1 -
	// distance/2 → distance = 2 * (1 - similarity). Pre-computing this
	// lets Lookup do an integer-cheap comparison per candidate.
	cosineCutoff := 2.0 * (1.0 - cfg.Threshold)
	return &SemanticCache{cfg: cfg, cosineCutoff: cosineCutoff, maxSimilarity: 1.0}, nil
}

// Lookup runs the full read pipeline. See Semantic.Lookup doc for the
// contract; this implementation surfaces every internal error other
// than "below threshold" / "empty input" so callers can distinguish
// "no match" from "backend down" and alert accordingly.
func (s *SemanticCache) Lookup(ctx context.Context, req *provider.ChatRequest) (*provider.ChatResponse, float64, error) {
	text := FlattenForEmbedding(req)
	if text == "" {
		return nil, 0, ErrMiss
	}

	vecs, err := s.cfg.Embedder.Embed(ctx, []string{text})
	if err != nil {
		return nil, 0, fmt.Errorf("cache: embed query: %w", err)
	}
	if len(vecs) != 1 {
		return nil, 0, fmt.Errorf("cache: embedder returned %d vectors, want 1", len(vecs))
	}

	matches, err := s.cfg.Index.Search(ctx, vecs[0], s.cfg.TopK)
	if err != nil {
		return nil, 0, fmt.Errorf("cache: semantic search: %w", err)
	}
	if len(matches) == 0 {
		return nil, 0, ErrMiss
	}

	best := matches[0] // Search returns ascending-distance / nearest-first
	similarity := s.maxSimilarity - best.Score/2.0
	if best.Score > s.cosineCutoff {
		// Closest neighbor is below the threshold: report the
		// similarity for caller-side logging, but signal a miss.
		return nil, similarity, ErrMiss
	}

	if len(best.Payload) == 0 {
		return nil, similarity, fmt.Errorf("cache: semantic match has empty payload")
	}
	var resp provider.ChatResponse
	if err := json.Unmarshal(best.Payload, &resp); err != nil {
		return nil, similarity, fmt.Errorf("cache: decode semantic payload: %w", err)
	}
	return &resp, similarity, nil
}

// Insert serializes resp and stores it under the request's flattened
// text. Empty flatten input is a no-op (the chat handler is expected
// to skip this call, but the guard keeps the contract symmetric with
// Lookup so misconfigured callers can't blow up here).
func (s *SemanticCache) Insert(ctx context.Context, req *provider.ChatRequest, resp *provider.ChatResponse) error {
	if resp == nil {
		return errors.New("cache: Insert: nil response")
	}
	text := FlattenForEmbedding(req)
	if text == "" {
		return nil
	}

	vecs, err := s.cfg.Embedder.Embed(ctx, []string{text})
	if err != nil {
		return fmt.Errorf("cache: embed insert: %w", err)
	}
	if len(vecs) != 1 {
		return fmt.Errorf("cache: embedder returned %d vectors on insert, want 1", len(vecs))
	}

	payload, err := json.Marshal(resp)
	if err != nil {
		return fmt.Errorf("cache: marshal semantic payload: %w", err)
	}

	// Use the exact-cache key as the stable id so cross-layer dedup is
	// possible. Doing so means a hit in semantic can be promoted into
	// the exact layer at the chat handler with no further keying work.
	key, err := Key(req)
	if err != nil {
		return fmt.Errorf("cache: derive key for semantic insert: %w", err)
	}

	if err := s.cfg.Index.Insert(ctx, key, vecs[0], payload); err != nil {
		return fmt.Errorf("cache: semantic index insert: %w", err)
	}
	return nil
}

// Compile-time interface assertion.
var _ Semantic = (*SemanticCache)(nil)
