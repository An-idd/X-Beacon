package cache

import (
	"context"
	"encoding/json"
	"errors"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/An-idd/x-beacon/internal/provider"
)

// fakeEmbedder is a deterministic Embedder for unit tests. A canned
// vector is returned per Embed call regardless of input — the tests
// drive the cache's behavior by varying what the index returns.
type fakeEmbedder struct {
	dim     int
	vec     []float32
	err     error
	calls   atomic.Int64
	lastIn  []string
}

func (f *fakeEmbedder) Embed(_ context.Context, texts []string) ([][]float32, error) {
	f.calls.Add(1)
	f.lastIn = append([]string(nil), texts...)
	if f.err != nil {
		return nil, f.err
	}
	out := make([][]float32, len(texts))
	for i := range texts {
		v := make([]float32, len(f.vec))
		copy(v, f.vec)
		out[i] = v
	}
	return out, nil
}
func (f *fakeEmbedder) Dimensions() int { return f.dim }
func (f *fakeEmbedder) Model() string   { return "fake" }

// fakeIndex is a SemanticIndex stub. Each test arms searchResults +
// searchErr; insertedKeys captures everything Insert was called with.
type fakeIndex struct {
	searchResults []SemanticMatch
	searchErr     error
	insertCalls   atomic.Int64
	insertedKeys  []string
	insertedVec   [][]float32
	insertedPayload [][]byte
	insertErr     error
}

func (f *fakeIndex) EnsureIndex(context.Context) error { return nil }
func (f *fakeIndex) Insert(_ context.Context, key string, vec []float32, payload []byte) error {
	f.insertCalls.Add(1)
	f.insertedKeys = append(f.insertedKeys, key)
	f.insertedVec = append(f.insertedVec, append([]float32(nil), vec...))
	f.insertedPayload = append(f.insertedPayload, append([]byte(nil), payload...))
	return f.insertErr
}
func (f *fakeIndex) Search(context.Context, []float32, int) ([]SemanticMatch, error) {
	return f.searchResults, f.searchErr
}
func (f *fakeIndex) Delete(context.Context, string) error { return nil }

// helper: construct a SemanticCache with the canned dependencies and
// the supplied threshold. dim defaults to 4 (small for tests).
func newTestSemantic(t *testing.T, threshold float64, idx *fakeIndex, emb *fakeEmbedder) *SemanticCache {
	t.Helper()
	if emb == nil {
		emb = &fakeEmbedder{dim: 4, vec: []float32{1, 0, 0, 0}}
	}
	if idx == nil {
		idx = &fakeIndex{}
	}
	c, err := NewSemanticCache(SemanticConfig{
		Embedder:  emb,
		Index:     idx,
		Threshold: threshold,
		TopK:      5,
	})
	require.NoError(t, err)
	return c
}

func TestNewSemanticCache_Validation(t *testing.T) {
	emb := &fakeEmbedder{dim: 4}
	idx := &fakeIndex{}

	cases := []struct {
		name string
		cfg  SemanticConfig
	}{
		{"nil embedder", SemanticConfig{Index: idx, Threshold: 0.9}},
		{"nil index", SemanticConfig{Embedder: emb, Threshold: 0.9}},
		{"threshold below 0", SemanticConfig{Embedder: emb, Index: idx, Threshold: -0.1}},
		{"threshold above 1", SemanticConfig{Embedder: emb, Index: idx, Threshold: 1.1}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := NewSemanticCache(tc.cfg)
			require.Error(t, err)
		})
	}
}

func TestNewSemanticCache_DefaultsTopK(t *testing.T) {
	c, err := NewSemanticCache(SemanticConfig{
		Embedder: &fakeEmbedder{dim: 4}, Index: &fakeIndex{}, Threshold: 0.9,
	})
	require.NoError(t, err)
	assert.Equal(t, 5, c.cfg.TopK)
}

// Lookup: empty flatten (no user message) → ErrMiss without touching
// the embedder or the index.
func TestSemanticLookup_EmptyFlattenSkipsBackends(t *testing.T) {
	emb := &fakeEmbedder{dim: 4, vec: []float32{1, 0, 0, 0}}
	idx := &fakeIndex{}
	c := newTestSemantic(t, 0.9, idx, emb)

	req := &provider.ChatRequest{
		Messages: []provider.Message{{Role: "system", Content: "be helpful"}}, // no user
	}
	resp, sim, err := c.Lookup(context.Background(), req)
	assert.True(t, errors.Is(err, ErrMiss))
	assert.Nil(t, resp)
	assert.Equal(t, 0.0, sim)
	assert.Equal(t, int64(0), emb.calls.Load(), "no embedder call on empty flatten")
}

// Lookup: index returns a match well above the threshold → hit.
// Verify the returned similarity matches the expected conversion.
func TestSemanticLookup_HitAboveThreshold(t *testing.T) {
	cached := &provider.ChatResponse{
		ID:    "cached-1",
		Model: "test-model",
		Choices: []provider.Choice{{
			Index:        0,
			Message:      provider.Message{Role: "assistant", Content: "hi from semantic"},
			FinishReason: "stop",
		}},
	}
	payload, err := json.Marshal(cached)
	require.NoError(t, err)

	idx := &fakeIndex{searchResults: []SemanticMatch{
		// distance 0.01 → similarity 1 - 0.005 = 0.995
		{Key: "k", Score: 0.01, Payload: payload},
	}}
	c := newTestSemantic(t, 0.95, idx, nil)

	req := &provider.ChatRequest{
		Model:    "test-model",
		Messages: []provider.Message{{Role: "user", Content: "hi"}},
	}
	got, sim, err := c.Lookup(context.Background(), req)
	require.NoError(t, err)
	require.NotNil(t, got)
	assert.Equal(t, "cached-1", got.ID)
	assert.InDelta(t, 0.995, sim, 1e-6)
}

// Lookup: closest neighbor below threshold → ErrMiss with a non-zero
// similarity for caller-side observability.
func TestSemanticLookup_BelowThresholdReportsSimilarity(t *testing.T) {
	idx := &fakeIndex{searchResults: []SemanticMatch{
		// distance 0.5 → similarity 1 - 0.25 = 0.75
		{Key: "k", Score: 0.5, Payload: []byte("{}")},
	}}
	c := newTestSemantic(t, 0.95, idx, nil)

	req := &provider.ChatRequest{
		Messages: []provider.Message{{Role: "user", Content: "x"}},
	}
	resp, sim, err := c.Lookup(context.Background(), req)
	assert.True(t, errors.Is(err, ErrMiss))
	assert.Nil(t, resp)
	assert.InDelta(t, 0.75, sim, 1e-6)
}

// Lookup: empty index → ErrMiss with similarity 0.
func TestSemanticLookup_EmptyIndex(t *testing.T) {
	c := newTestSemantic(t, 0.95, &fakeIndex{searchResults: nil}, nil)
	req := &provider.ChatRequest{
		Messages: []provider.Message{{Role: "user", Content: "x"}},
	}
	_, sim, err := c.Lookup(context.Background(), req)
	assert.True(t, errors.Is(err, ErrMiss))
	assert.Equal(t, 0.0, sim)
}

// Lookup: embedder error propagates (caller decides whether to log
// noisy or quiet).
func TestSemanticLookup_EmbedErrorSurfaces(t *testing.T) {
	emb := &fakeEmbedder{dim: 4, vec: []float32{1, 0, 0, 0}, err: errors.New("upstream embed down")}
	c := newTestSemantic(t, 0.95, &fakeIndex{}, emb)
	req := &provider.ChatRequest{
		Messages: []provider.Message{{Role: "user", Content: "x"}},
	}
	_, _, err := c.Lookup(context.Background(), req)
	require.Error(t, err)
	assert.False(t, errors.Is(err, ErrMiss))
}

// Lookup: index search error propagates similarly.
func TestSemanticLookup_SearchErrorSurfaces(t *testing.T) {
	idx := &fakeIndex{searchErr: errors.New("redis down")}
	c := newTestSemantic(t, 0.95, idx, nil)
	req := &provider.ChatRequest{
		Messages: []provider.Message{{Role: "user", Content: "x"}},
	}
	_, _, err := c.Lookup(context.Background(), req)
	require.Error(t, err)
	assert.False(t, errors.Is(err, ErrMiss))
}

// Lookup: malformed payload → error (NOT a miss). Corrupted entries
// are an alert signal — they typically mean a schema mismatch.
func TestSemanticLookup_MalformedPayloadIsError(t *testing.T) {
	idx := &fakeIndex{searchResults: []SemanticMatch{
		{Key: "k", Score: 0.01, Payload: []byte("not-json{{")},
	}}
	c := newTestSemantic(t, 0.95, idx, nil)
	req := &provider.ChatRequest{
		Messages: []provider.Message{{Role: "user", Content: "x"}},
	}
	_, _, err := c.Lookup(context.Background(), req)
	require.Error(t, err)
	assert.False(t, errors.Is(err, ErrMiss))
}

// Insert happy path: ChatResponse marshalled, vec computed, Insert
// called with the exact-cache key.
func TestSemanticInsert_HappyPath(t *testing.T) {
	idx := &fakeIndex{}
	emb := &fakeEmbedder{dim: 4, vec: []float32{0.5, 0.5, 0.5, 0.5}}
	c := newTestSemantic(t, 0.95, idx, emb)

	req := &provider.ChatRequest{
		Model:    "test-model",
		Messages: []provider.Message{{Role: "user", Content: "hello"}},
	}
	resp := &provider.ChatResponse{
		ID:    "r1",
		Model: "test-model",
		Choices: []provider.Choice{{
			Index:        0,
			Message:      provider.Message{Role: "assistant", Content: "hey"},
			FinishReason: "stop",
		}},
	}
	require.NoError(t, c.Insert(context.Background(), req, resp))
	require.Equal(t, int64(1), idx.insertCalls.Load())

	// Same key as the exact cache layer would use — proves cross-layer
	// dedup is feasible.
	expected, err := Key(req)
	require.NoError(t, err)
	assert.Equal(t, []string{expected}, idx.insertedKeys)

	// Round-trip the payload to confirm marshaling.
	require.Len(t, idx.insertedPayload, 1)
	var got provider.ChatResponse
	require.NoError(t, json.Unmarshal(idx.insertedPayload[0], &got))
	assert.Equal(t, "r1", got.ID)
}

func TestSemanticInsert_NilResponseRejected(t *testing.T) {
	c := newTestSemantic(t, 0.95, &fakeIndex{}, nil)
	err := c.Insert(context.Background(), &provider.ChatRequest{}, nil)
	require.Error(t, err)
}

// Insert: empty flatten is a silent no-op so the chat handler can
// call this unconditionally without needing to inspect the request.
func TestSemanticInsert_EmptyFlattenIsNoop(t *testing.T) {
	idx := &fakeIndex{}
	emb := &fakeEmbedder{dim: 4, vec: []float32{1, 0, 0, 0}}
	c := newTestSemantic(t, 0.95, idx, emb)
	req := &provider.ChatRequest{
		Messages: []provider.Message{{Role: "system", Content: "no user turn"}},
	}
	require.NoError(t, c.Insert(context.Background(), req, &provider.ChatResponse{}))
	assert.Equal(t, int64(0), idx.insertCalls.Load(), "no insert on empty flatten")
	assert.Equal(t, int64(0), emb.calls.Load(), "no embed on empty flatten")
}
