package cache

import (
	"context"
	"errors"
	"os"
	"testing"

	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestNewRedisSemanticIndex_RequiresClient(t *testing.T) {
	_, err := NewRedisSemanticIndex(nil, SemanticIndexConfig{Dimensions: 4})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "redis client")
}

func TestNewRedisSemanticIndex_RequiresPositiveDimensions(t *testing.T) {
	client := redis.NewClient(&redis.Options{Addr: "127.0.0.1:0"})
	t.Cleanup(func() { _ = client.Close() })

	_, err := NewRedisSemanticIndex(client, SemanticIndexConfig{})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "Dimensions")

	_, err = NewRedisSemanticIndex(client, SemanticIndexConfig{Dimensions: -1})
	require.Error(t, err)
}

func TestNewRedisSemanticIndex_DefaultsApplied(t *testing.T) {
	client := redis.NewClient(&redis.Options{Addr: "127.0.0.1:0"})
	t.Cleanup(func() { _ = client.Close() })

	idx, err := NewRedisSemanticIndex(client, SemanticIndexConfig{Dimensions: 1536})
	require.NoError(t, err)
	assert.Equal(t, defaultIndexName, idx.cfg.IndexName)
	assert.Equal(t, defaultKeyPrefix, idx.cfg.KeyPrefix)
	assert.Equal(t, defaultDistance, idx.cfg.Distance)
	assert.Equal(t, defaultHNSWM, idx.cfg.HNSWM)
	assert.Equal(t, defaultHNSWEFConstruction, idx.cfg.HNSWEFConstruction)
}

func TestEncodeDecodeVector_RoundTrip(t *testing.T) {
	in := []float32{0.0, 1.5, -2.25, 3.14159, 1e-10}
	enc := EncodeVector(in)
	require.Equal(t, len(in)*4, len(enc), "4 bytes per float32")

	out, err := DecodeVector(enc)
	require.NoError(t, err)
	assert.Equal(t, in, out)
}

func TestDecodeVector_BadLength(t *testing.T) {
	_, err := DecodeVector([]byte{1, 2, 3})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "multiple of 4")
}

func TestCreateArgs_Shape(t *testing.T) {
	client := redis.NewClient(&redis.Options{Addr: "127.0.0.1:0"})
	t.Cleanup(func() { _ = client.Close() })

	idx, err := NewRedisSemanticIndex(client, SemanticIndexConfig{Dimensions: 1536})
	require.NoError(t, err)

	args := idx.createArgs()
	// Spot-check the critical positions; full string comparison would
	// be brittle to RediSearch arg additions.
	assert.Equal(t, "FT.CREATE", args[0])
	assert.Equal(t, defaultIndexName, args[1])
	assert.Contains(t, args, "VECTOR")
	assert.Contains(t, args, "HNSW")
	assert.Contains(t, args, "FLOAT32")
	assert.Contains(t, args, "1536") // DIM
	assert.Contains(t, args, "COSINE")
}

func TestSearchArgs_BlobInPayloadParams(t *testing.T) {
	client := redis.NewClient(&redis.Options{Addr: "127.0.0.1:0"})
	t.Cleanup(func() { _ = client.Close() })

	idx, err := NewRedisSemanticIndex(client, SemanticIndexConfig{Dimensions: 4})
	require.NoError(t, err)

	vec := []float32{0.1, 0.2, 0.3, 0.4}
	args := idx.searchArgs(vec, 5)

	// Required clauses for KNN search via DIALECT 2.
	assert.Equal(t, "FT.SEARCH", args[0])
	assert.Equal(t, defaultIndexName, args[1])
	assert.Contains(t, args[2], "[KNN 5 @vec $vec_param AS score]")
	assert.Contains(t, args, "PARAMS")
	assert.Contains(t, args, "DIALECT")
	assert.Contains(t, args, "2")
	// Blob is the encoded float32 bytes, not the raw slice.
	assert.Contains(t, args, EncodeVector(vec))
}

func TestInsert_RejectsDimensionMismatch(t *testing.T) {
	client := redis.NewClient(&redis.Options{Addr: "127.0.0.1:0"})
	t.Cleanup(func() { _ = client.Close() })

	idx, err := NewRedisSemanticIndex(client, SemanticIndexConfig{Dimensions: 4})
	require.NoError(t, err)

	err = idx.Insert(context.Background(), "k", []float32{1, 2, 3}, []byte("p"))
	require.Error(t, err)
	assert.True(t, errors.Is(err, ErrDimensionMismatch))
}

func TestSearch_RejectsInvalidTopK(t *testing.T) {
	client := redis.NewClient(&redis.Options{Addr: "127.0.0.1:0"})
	t.Cleanup(func() { _ = client.Close() })

	idx, err := NewRedisSemanticIndex(client, SemanticIndexConfig{Dimensions: 4})
	require.NoError(t, err)

	_, err = idx.Search(context.Background(), []float32{1, 2, 3, 4}, 0)
	assert.True(t, errors.Is(err, ErrInvalidTopK))
}

func TestSearch_RejectsDimensionMismatch(t *testing.T) {
	client := redis.NewClient(&redis.Options{Addr: "127.0.0.1:0"})
	t.Cleanup(func() { _ = client.Close() })

	idx, err := NewRedisSemanticIndex(client, SemanticIndexConfig{Dimensions: 4})
	require.NoError(t, err)

	_, err = idx.Search(context.Background(), []float32{1, 2}, 5)
	assert.True(t, errors.Is(err, ErrDimensionMismatch))
}

func TestParseSearchReply_HappyPath(t *testing.T) {
	// Shape: [total, key, [score, "0.1", payload, "{hello}", cache_key, "abc"]]
	raw := []any{
		int64(1),
		"cache:sem:abc",
		[]any{
			"score", "0.1",
			"payload", "{\"id\":\"x\"}",
			"cache_key", "abc",
		},
	}
	matches, err := parseSearchReply(raw)
	require.NoError(t, err)
	require.Len(t, matches, 1)
	assert.Equal(t, "abc", matches[0].Key, "cache_key field overrides the hash key form")
	assert.InDelta(t, 0.1, matches[0].Score, 1e-9)
	assert.Equal(t, []byte(`{"id":"x"}`), matches[0].Payload)
}

func TestParseSearchReply_EmptyZeroResult(t *testing.T) {
	matches, err := parseSearchReply([]any{int64(0)})
	require.NoError(t, err)
	assert.Empty(t, matches)
}

func TestParseSearchReply_BadTopLevelType(t *testing.T) {
	_, err := parseSearchReply("not-an-array")
	require.Error(t, err)
}

func TestParseSearchReply_RESP3MapShape(t *testing.T) {
	// Shape from redis-stack 7.4: top-level map with results array of
	// per-entry maps holding extra_attributes.
	raw := map[any]any{
		"total_results": int64(1),
		"results": []any{
			map[any]any{
				"id": "cache:sem:abc",
				"extra_attributes": map[any]any{
					"score":     "0.0001",
					"payload":   "{\"id\":\"x\"}",
					"cache_key": "abc",
				},
			},
		},
	}
	matches, err := parseSearchReply(raw)
	require.NoError(t, err)
	require.Len(t, matches, 1)
	assert.Equal(t, "abc", matches[0].Key)
	assert.InDelta(t, 0.0001, matches[0].Score, 1e-9)
	assert.Equal(t, []byte(`{"id":"x"}`), matches[0].Payload)
}

func TestParseSearchReply_RESP3MapEmptyResults(t *testing.T) {
	raw := map[any]any{"total_results": int64(0)}
	matches, err := parseSearchReply(raw)
	require.NoError(t, err)
	assert.Empty(t, matches)
}

// ---- Integration: real Redis Stack -------------------------------
//
// Gated by XBEACON_TEST_REDIS_STACK_ADDR (e.g. "127.0.0.1:6379"). The
// docker-compose.yml ships redis-stack-server; CI without it skips
// this test instead of failing. Plain redis:7-alpine without the
// search module will surface as an FT.CREATE error here, which is the
// signal we want.

func semanticIntegrationAddr() string {
	if v := os.Getenv("XBEACON_TEST_REDIS_STACK_ADDR"); v != "" {
		return v
	}
	return ""
}

func TestRedisSemanticIndex_EndToEnd_Integration(t *testing.T) {
	addr := semanticIntegrationAddr()
	if addr == "" {
		t.Skip("XBEACON_TEST_REDIS_STACK_ADDR not set; skipping RediSearch integration test")
	}

	client := redis.NewClient(&redis.Options{Addr: addr})
	t.Cleanup(func() { _ = client.Close() })

	if err := client.Ping(context.Background()).Err(); err != nil {
		t.Fatalf("connect %s: %v", addr, err)
	}

	// Use a test-unique index name + key prefix so reruns don't
	// collide and the cleanup can target what we created.
	cfg := SemanticIndexConfig{
		IndexName:  "x_beacon_test_semcache",
		KeyPrefix:  "x_beacon_test:sem:",
		Dimensions: 4,
	}
	idx, err := NewRedisSemanticIndex(client, cfg)
	require.NoError(t, err)

	// Cleanup any leftover index before / after.
	_ = client.Do(context.Background(), "FT.DROPINDEX", cfg.IndexName, "DD").Err()
	t.Cleanup(func() {
		_ = client.Do(context.Background(), "FT.DROPINDEX", cfg.IndexName, "DD").Err()
	})

	require.NoError(t, idx.EnsureIndex(context.Background()))
	// Idempotency: second EnsureIndex must succeed.
	require.NoError(t, idx.EnsureIndex(context.Background()))

	require.NoError(t, idx.Insert(context.Background(), "alpha",
		[]float32{1, 0, 0, 0}, []byte(`{"id":"alpha"}`)))
	require.NoError(t, idx.Insert(context.Background(), "beta",
		[]float32{0, 1, 0, 0}, []byte(`{"id":"beta"}`)))

	// Querying with a vector close to alpha must rank alpha first.
	matches, err := idx.Search(context.Background(),
		[]float32{0.99, 0.01, 0, 0}, 2)
	require.NoError(t, err)
	require.Len(t, matches, 2)
	assert.Equal(t, "alpha", matches[0].Key)
	assert.Equal(t, []byte(`{"id":"alpha"}`), matches[0].Payload)
	assert.LessOrEqual(t, matches[0].Score, matches[1].Score, "results must be sorted ascending")

	require.NoError(t, idx.Delete(context.Background(), "alpha"))
	matches, err = idx.Search(context.Background(),
		[]float32{0.99, 0.01, 0, 0}, 2)
	require.NoError(t, err)
	require.Len(t, matches, 1)
	assert.Equal(t, "beta", matches[0].Key)
}
