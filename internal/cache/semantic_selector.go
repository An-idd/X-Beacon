package cache

import (
	"context"
	"errors"
	"fmt"
	"sync"

	"github.com/redis/go-redis/v9"

	"github.com/An-idd/x-beacon/internal/provider"
	"github.com/An-idd/x-beacon/pkg/embedding"
)

// SemanticSelector dispatches Lookup/Insert calls to per-model
// SemanticCache instances. Each model gets its own RediSearch
// IndexName under the configured prefix; entries from different
// models never share a vector graph, so KNN can't return
// cross-model results regardless of similarity.
//
// Implementation notes:
//
//   - Per-model SemanticCache is created lazily on first request for
//     that model. Construction calls EnsureIndex() so subsequent
//     requests skip the FT.CREATE round trip.
//   - The shared Embedder is wrapped once at construction (typically
//     with LRU). It's safe to share — the LRU keys on input text, not
//     on model.
//   - Concurrency: one sync.RWMutex guards the model→cache map; the
//     fast path (model already known) is read-locked. New-model
//     creation takes the write lock briefly.
//
// Decision 2 (Phase 3): per-model IndexName chosen over single-index
// + TAG filter to (a) prevent cross-model bleed in KNN results and
// (b) preserve native HNSW performance on long-tail models.
type SemanticSelector struct {
	cfg SemanticSelectorConfig

	mu     sync.RWMutex
	caches map[string]*SemanticCache
}

// SemanticSelectorConfig configures the per-model dispatcher.
type SemanticSelectorConfig struct {
	// Redis client used to construct each per-model SemanticIndex.
	Redis *redis.Client

	// Embedder shared across all per-model caches. Typically an
	// OpenAI client wrapped in pkg/embedding.LRUEmbedder.
	Embedder embedding.Embedder

	// Threshold (cosine similarity 0..1) applied uniformly across
	// models. Per-model overrides not supported in Week 10.
	Threshold float64

	// TopK controls how many neighbors each KNN call returns. Default 5.
	TopK int

	// IndexNamePrefix is the FT index name prefix; the full name is
	// "<prefix><sanitized-model>". Default "x_beacon_semcache_".
	IndexNamePrefix string

	// KeyPrefix is the HSET key prefix for each per-model index.
	// Default "cache:sem:". Per-model isolation comes from IndexName,
	// not key prefix; the prefix is shared so dropping the index
	// reclaims the keyspace cleanly via FT.DROPINDEX...DD.
	KeyPrefix string

	// HNSWM / HNSWEFConstruction passed through to RediSearch index
	// creation. Zero means "use RediSearch default".
	HNSWM              int
	HNSWEFConstruction int
}

// NewSemanticSelector validates the config and returns a ready
// dispatcher. Per-model indices are NOT created here — each is built
// on first request for that model so dev-mode + unused models cost
// nothing.
func NewSemanticSelector(cfg SemanticSelectorConfig) (*SemanticSelector, error) {
	if cfg.Redis == nil {
		return nil, errors.New("cache: SemanticSelector requires non-nil Redis client")
	}
	if cfg.Embedder == nil {
		return nil, errors.New("cache: SemanticSelector requires non-nil Embedder")
	}
	if cfg.Threshold < 0 || cfg.Threshold > 1 {
		return nil, fmt.Errorf("cache: SemanticSelector.Threshold out of range [0,1]: %v", cfg.Threshold)
	}
	if cfg.TopK <= 0 {
		cfg.TopK = 5
	}
	if cfg.IndexNamePrefix == "" {
		cfg.IndexNamePrefix = "x_beacon_semcache_"
	}
	if cfg.KeyPrefix == "" {
		cfg.KeyPrefix = defaultKeyPrefix
	}
	return &SemanticSelector{
		cfg:    cfg,
		caches: make(map[string]*SemanticCache),
	}, nil
}

// Lookup routes to the per-model cache. An empty model name returns
// ErrMiss — we won't speculatively create indices for malformed
// requests.
func (s *SemanticSelector) Lookup(ctx context.Context, req *provider.ChatRequest) (*provider.ChatResponse, float64, error) {
	if req == nil || req.Model == "" {
		return nil, 0, ErrMiss
	}
	cache, err := s.cacheFor(ctx, req.Model)
	if err != nil {
		return nil, 0, err
	}
	return cache.Lookup(ctx, req)
}

// Insert routes to the per-model cache. Empty model is rejected
// (calling Insert on a malformed request would create a phantom
// index, polluting RediSearch's catalog).
func (s *SemanticSelector) Insert(ctx context.Context, req *provider.ChatRequest, resp *provider.ChatResponse) error {
	if req == nil || req.Model == "" {
		return errors.New("cache: SemanticSelector.Insert requires req.Model")
	}
	cache, err := s.cacheFor(ctx, req.Model)
	if err != nil {
		return err
	}
	return cache.Insert(ctx, req, resp)
}

// Models returns the list of models for which a per-model cache has
// been created. Order is not stable; primarily for ops/debug surfaces.
func (s *SemanticSelector) Models() []string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]string, 0, len(s.caches))
	for m := range s.caches {
		out = append(out, m)
	}
	return out
}

// cacheFor lazily creates the per-model cache. Double-checked locking
// keeps the steady-state path read-locked.
func (s *SemanticSelector) cacheFor(ctx context.Context, model string) (*SemanticCache, error) {
	s.mu.RLock()
	if c, ok := s.caches[model]; ok {
		s.mu.RUnlock()
		return c, nil
	}
	s.mu.RUnlock()

	s.mu.Lock()
	defer s.mu.Unlock()
	if c, ok := s.caches[model]; ok {
		return c, nil
	}

	indexName := s.cfg.IndexNamePrefix + sanitizeIndexName(model)
	idx, err := NewRedisSemanticIndex(s.cfg.Redis, SemanticIndexConfig{
		IndexName:          indexName,
		KeyPrefix:          s.cfg.KeyPrefix + sanitizeIndexName(model) + ":",
		Dimensions:         s.cfg.Embedder.Dimensions(),
		HNSWM:              s.cfg.HNSWM,
		HNSWEFConstruction: s.cfg.HNSWEFConstruction,
	})
	if err != nil {
		return nil, fmt.Errorf("cache: build per-model index for %q: %w", model, err)
	}
	if err := idx.EnsureIndex(ctx); err != nil {
		return nil, fmt.Errorf("cache: ensure index %s: %w", indexName, err)
	}
	c, err := NewSemanticCache(SemanticConfig{
		Embedder:  s.cfg.Embedder,
		Index:     idx,
		Threshold: s.cfg.Threshold,
		TopK:      s.cfg.TopK,
	})
	if err != nil {
		return nil, fmt.Errorf("cache: build per-model cache for %q: %w", model, err)
	}
	s.caches[model] = c
	return c, nil
}

// sanitizeIndexName trims model identifiers to RediSearch-safe
// characters. RediSearch doesn't enforce strict naming but `:` and
// `/` confuse some shell tooling; we keep it conservative.
func sanitizeIndexName(model string) string {
	out := make([]byte, 0, len(model))
	for i := 0; i < len(model); i++ {
		c := model[i]
		switch {
		case c >= 'a' && c <= 'z',
			c >= 'A' && c <= 'Z',
			c >= '0' && c <= '9',
			c == '_', c == '-':
			out = append(out, c)
		default:
			out = append(out, '_')
		}
	}
	if len(out) == 0 {
		return "default"
	}
	return string(out)
}

// Compile-time interface guard.
var _ Semantic = (*SemanticSelector)(nil)
