package cache

import (
	"context"
	"errors"
	"testing"

	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/An-idd/x-beacon/internal/provider"
)

func TestSanitizeIndexName(t *testing.T) {
	cases := map[string]string{
		"gpt-4o":            "gpt-4o",
		"claude-3-5-sonnet": "claude-3-5-sonnet",
		"a/b:c.d":           "a_b_c_d",
		"foo bar":           "foo_bar",
		"":                  "default",
	}
	for in, want := range cases {
		assert.Equal(t, want, sanitizeIndexName(in), "input=%q", in)
	}
}

func TestNewSemanticSelector_Validation(t *testing.T) {
	rdb := redis.NewClient(&redis.Options{Addr: "127.0.0.1:0"})
	t.Cleanup(func() { _ = rdb.Close() })
	emb := &fakeEmbedder{dim: 4}

	cases := []struct {
		name string
		cfg  SemanticSelectorConfig
	}{
		{"nil redis", SemanticSelectorConfig{Embedder: emb, Threshold: 0.9}},
		{"nil embedder", SemanticSelectorConfig{Redis: rdb, Threshold: 0.9}},
		{"threshold < 0", SemanticSelectorConfig{Redis: rdb, Embedder: emb, Threshold: -0.1}},
		{"threshold > 1", SemanticSelectorConfig{Redis: rdb, Embedder: emb, Threshold: 1.1}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := NewSemanticSelector(tc.cfg)
			require.Error(t, err)
		})
	}
}

func TestNewSemanticSelector_DefaultsApplied(t *testing.T) {
	rdb := redis.NewClient(&redis.Options{Addr: "127.0.0.1:0"})
	t.Cleanup(func() { _ = rdb.Close() })
	s, err := NewSemanticSelector(SemanticSelectorConfig{
		Redis: rdb, Embedder: &fakeEmbedder{dim: 4}, Threshold: 0.9,
	})
	require.NoError(t, err)
	assert.Equal(t, "x_beacon_semcache_", s.cfg.IndexNamePrefix)
	assert.Equal(t, defaultKeyPrefix, s.cfg.KeyPrefix)
	assert.Equal(t, 5, s.cfg.TopK)
}

// LookupWithEmptyModel returns ErrMiss without touching the per-model
// machinery — guards against speculative index creation for malformed
// requests.
func TestSemanticSelector_LookupEmptyModel(t *testing.T) {
	rdb := redis.NewClient(&redis.Options{Addr: "127.0.0.1:0"})
	t.Cleanup(func() { _ = rdb.Close() })
	s, err := NewSemanticSelector(SemanticSelectorConfig{
		Redis: rdb, Embedder: &fakeEmbedder{dim: 4}, Threshold: 0.9,
	})
	require.NoError(t, err)

	_, _, err = s.Lookup(context.Background(), nil)
	assert.True(t, errors.Is(err, ErrMiss))

	_, _, err = s.Lookup(context.Background(), &fakeChatReqEmptyModel)
	assert.True(t, errors.Is(err, ErrMiss))
}

func TestSemanticSelector_InsertEmptyModelRejected(t *testing.T) {
	rdb := redis.NewClient(&redis.Options{Addr: "127.0.0.1:0"})
	t.Cleanup(func() { _ = rdb.Close() })
	s, err := NewSemanticSelector(SemanticSelectorConfig{
		Redis: rdb, Embedder: &fakeEmbedder{dim: 4}, Threshold: 0.9,
	})
	require.NoError(t, err)
	err = s.Insert(context.Background(), &fakeChatReqEmptyModel, nil)
	require.Error(t, err)
}

// fakeChatReqEmptyModel is a request with no Model field. Used by
// the validation tests above.
var fakeChatReqEmptyModel = provider.ChatRequest{
	Messages: []provider.Message{{Role: "user", Content: "x"}},
}

// TestSemanticSelector_PerModelIsolation_Integration: same flatten
// text + same embedding vector under two different models must NOT
// cross-bleed. Asserts that the selector creates two distinct indices
// and KNN never returns gpt-4o entries for a claude lookup (the bug
// that motivated Decision 2).
//
// Gated by XBEACON_TEST_REDIS_STACK_ADDR — needs RediSearch.
func TestSemanticSelector_PerModelIsolation_Integration(t *testing.T) {
	addr := semanticIntegrationAddr()
	if addr == "" {
		t.Skip("XBEACON_TEST_REDIS_STACK_ADDR not set; skipping RediSearch integration test")
	}
	rdb := redis.NewClient(&redis.Options{Addr: addr})
	t.Cleanup(func() { _ = rdb.Close() })

	prefix := "x_beacon_test_isol_"
	t.Cleanup(func() {
		// Cleanup any indices we created.
		for _, m := range []string{"test-gpt-4o", "test-claude-3"} {
			_ = rdb.Do(context.Background(), "FT.DROPINDEX", prefix+sanitizeIndexName(m), "DD").Err()
		}
	})

	// Pre-cleanup in case of a stale prior run.
	for _, m := range []string{"test-gpt-4o", "test-claude-3"} {
		_ = rdb.Do(context.Background(), "FT.DROPINDEX", prefix+sanitizeIndexName(m), "DD").Err()
	}

	emb := &fakeEmbedder{dim: 4, vec: []float32{1, 0, 0, 0}}
	sel, err := NewSemanticSelector(SemanticSelectorConfig{
		Redis:           rdb,
		Embedder:        emb,
		Threshold:       0.95,
		IndexNamePrefix: prefix,
		KeyPrefix:       "x_beacon_test_isol:",
	})
	require.NoError(t, err)

	// Insert under model A; identical flatten text under model B.
	reqA := &provider.ChatRequest{
		Model:    "test-gpt-4o",
		Messages: []provider.Message{{Role: "user", Content: "explain pointers"}},
	}
	reqB := &provider.ChatRequest{
		Model:    "test-claude-3",
		Messages: []provider.Message{{Role: "user", Content: "explain pointers"}},
	}
	respA := &provider.ChatResponse{
		ID: "from-a", Model: "test-gpt-4o",
		Choices: []provider.Choice{{Index: 0, Message: provider.Message{Role: "assistant", Content: "A's answer"}, FinishReason: "stop"}},
	}
	require.NoError(t, sel.Insert(context.Background(), reqA, respA))

	// Lookup under model B: same flatten, same vec — but DIFFERENT
	// index → must miss.
	resp, _, err := sel.Lookup(context.Background(), reqB)
	assert.True(t, errors.Is(err, ErrMiss),
		"per-model isolation: model B lookup must NOT see model A entry; got resp=%v err=%v", resp, err)

	// Lookup under model A: same flatten, same vec — same index →
	// must hit, returns A's response.
	resp, sim, err := sel.Lookup(context.Background(), reqA)
	require.NoError(t, err)
	require.NotNil(t, resp)
	assert.Equal(t, "from-a", resp.ID)
	assert.InDelta(t, 1.0, sim, 1e-6)

	// Models() reports both even though only A has entries (both
	// indices were created on first request).
	models := sel.Models()
	assert.ElementsMatch(t, []string{"test-gpt-4o", "test-claude-3"}, models)
}
