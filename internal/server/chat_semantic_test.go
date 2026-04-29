package server

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/An-idd/x-beacon/internal/cache"
	"github.com/An-idd/x-beacon/internal/observability"
)

// stubEmbedder always returns vec for any input. Lets the test
// guarantee semantic-hit vs miss based purely on whether the index
// has a sufficiently-close vector pre-seeded.
type stubEmbedder struct {
	vec []float32
}

func (e *stubEmbedder) Embed(_ context.Context, texts []string) ([][]float32, error) {
	out := make([][]float32, len(texts))
	for i := range texts {
		v := make([]float32, len(e.vec))
		copy(v, e.vec)
		out[i] = v
	}
	return out, nil
}
func (e *stubEmbedder) Dimensions() int { return len(e.vec) }
func (e *stubEmbedder) Model() string   { return "stub" }

// stubIndex is a SemanticIndex stub keyed in memory. Insert writes
// vector + payload into a slice; Search runs an O(n) cosine distance
// to mimic the real RediSearch ranking, returning sorted-by-distance.
type stubIndex struct {
	dim   int
	rows  []stubRow
}

type stubRow struct {
	key     string
	vec     []float32
	payload []byte
}

func (s *stubIndex) EnsureIndex(context.Context) error { return nil }
func (s *stubIndex) Insert(_ context.Context, key string, vec []float32, payload []byte) error {
	v := make([]float32, len(vec))
	copy(v, vec)
	p := make([]byte, len(payload))
	copy(p, payload)
	// Replace if key already exists.
	for i, r := range s.rows {
		if r.key == key {
			s.rows[i] = stubRow{key: key, vec: v, payload: p}
			return nil
		}
	}
	s.rows = append(s.rows, stubRow{key: key, vec: v, payload: p})
	return nil
}
func (s *stubIndex) Delete(_ context.Context, key string) error {
	for i, r := range s.rows {
		if r.key == key {
			s.rows = append(s.rows[:i], s.rows[i+1:]...)
			return nil
		}
	}
	return nil
}
func (s *stubIndex) Search(_ context.Context, vec []float32, topK int) ([]cache.SemanticMatch, error) {
	matches := make([]cache.SemanticMatch, 0, len(s.rows))
	for _, r := range s.rows {
		// Cosine distance for parallel/identical vectors is 0; we use
		// the stub's identity for stable test behavior.
		var dot float32
		for i := range vec {
			dot += vec[i] * r.vec[i]
		}
		// Map to [0,2]: identical vec → distance 0.
		dist := 2 - 2*float64(dot)
		if dist < 0 {
			dist = 0
		}
		matches = append(matches, cache.SemanticMatch{Key: r.key, Score: dist, Payload: r.payload})
	}
	// Sort ascending by Score (nearest first).
	for i := 1; i < len(matches); i++ {
		for j := i; j > 0 && matches[j].Score < matches[j-1].Score; j-- {
			matches[j], matches[j-1] = matches[j-1], matches[j]
		}
	}
	if topK > len(matches) {
		topK = len(matches)
	}
	return matches[:topK], nil
}

// newSemanticChatFixture builds a chat handler wired with a stub
// embedder + stub index + miniredis-backed exact cache.
type semChatFixture struct {
	srv      *Server
	exact    cache.Exact
	semantic *cache.SemanticCache
	upstream *atomic.Int64
}

func newSemanticChatFixture(t *testing.T) *semChatFixture {
	t.Helper()
	upstreamHits := &atomic.Int64{}
	upstream := func(w http.ResponseWriter, _ *http.Request) {
		upstreamHits.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{
			"id":"upstream-1","object":"chat.completion","created":1714000000,"model":"test-model",
			"choices":[{"index":0,"message":{"role":"assistant","content":"upstream answer"},"finish_reason":"stop"}],
			"usage":{"prompt_tokens":3,"completion_tokens":2,"total_tokens":5}
		}`))
	}
	reg, _ := buildRegistry(t, upstream)

	mr := miniredis.RunT(t)
	client := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = client.Close() })
	exact := cache.NewRedisExact(client)

	embedder := &stubEmbedder{vec: []float32{1, 0, 0, 0}}
	idx := &stubIndex{dim: 4}
	sc, err := cache.NewSemanticCache(cache.SemanticConfig{
		Embedder:  embedder,
		Index:     idx,
		Threshold: 0.95,
		TopK:      5,
	})
	require.NoError(t, err)

	srv := newTestServer(t, func(d *Deps) {
		d.Registry = reg
		d.Router = newTestRouter(reg)
		d.Cache = exact
		d.CacheTTL = time.Minute
		d.Semantic = sc
	})
	return &semChatFixture{srv: srv, exact: exact, semantic: sc, upstream: upstreamHits}
}

func (f *semChatFixture) post(t *testing.T, body []byte) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	f.srv.Handler().ServeHTTP(rec, req)
	return rec
}

// First request: exact miss + semantic miss → upstream → both caches
// written. Asserted via "miss" header on the first call.
func TestChat_SemanticMiss_FillsBothLayers(t *testing.T) {
	f := newSemanticChatFixture(t)

	rec := f.post(t, chatBody("test-model", "explain pointers", false))
	require.Equal(t, http.StatusOK, rec.Code)
	assert.Equal(t, "miss", rec.Header().Get("X-X-Beacon-Cache"))
	assert.Equal(t, int64(1), f.upstream.Load())

	// Second identical request: exact hit (preferred over semantic).
	rec2 := f.post(t, chatBody("test-model", "explain pointers", false))
	assert.Equal(t, "hit", rec2.Header().Get("X-X-Beacon-Cache"))
	assert.Empty(t, rec2.Header().Get("X-X-Beacon-Cache-Layer"),
		"exact hit must NOT carry the semantic layer header")
	assert.Equal(t, int64(1), f.upstream.Load())
}

// A request with different content but flatten-similar text hits
// semantic (because stubEmbedder returns the same vec regardless of
// input). Verifies the layer header + that exact stayed empty for the
// new key.
func TestChat_SemanticHit_DifferentContentSameVec(t *testing.T) {
	f := newSemanticChatFixture(t)

	// Warm: the exact cache won't have the second request's key, but
	// the semantic index will have a matching vector from this insert.
	first := f.post(t, chatBody("test-model", "alpha question", false))
	require.Equal(t, http.StatusOK, first.Code)
	assert.Equal(t, "miss", first.Header().Get("X-X-Beacon-Cache"))

	// Different content (different exact key) but stubEmbedder returns
	// the same vec → semantic KNN finds the warm row at distance 0 →
	// hit.
	second := f.post(t, chatBody("test-model", "beta question entirely different", false))
	assert.Equal(t, "hit", second.Header().Get("X-X-Beacon-Cache"))
	assert.Equal(t, "semantic", second.Header().Get("X-X-Beacon-Cache-Layer"))
	assert.Equal(t, int64(1), f.upstream.Load(),
		"semantic hit must not contact upstream a second time")
}

// Decision 6 / Option B: semantic hit must NOT promote into exact.
// Verified by issuing a third request with the same body as the
// second — if promotion occurred, this would be an *exact* hit
// (no layer header), but Option B keeps it as semantic.
func TestChat_SemanticHit_DoesNotPromoteToExact(t *testing.T) {
	f := newSemanticChatFixture(t)

	_ = f.post(t, chatBody("test-model", "question A", false))

	// Different content → semantic hit.
	r1 := f.post(t, chatBody("test-model", "question B", false))
	require.Equal(t, "semantic", r1.Header().Get("X-X-Beacon-Cache-Layer"))

	// Same body again → still a semantic hit, NOT an exact hit, because
	// Option B does not promote.
	r2 := f.post(t, chatBody("test-model", "question B", false))
	assert.Equal(t, "hit", r2.Header().Get("X-X-Beacon-Cache"))
	assert.Equal(t, "semantic", r2.Header().Get("X-X-Beacon-Cache-Layer"),
		"Option B: semantic hits must not promote to exact")
}

// Hit body equals what was originally cached (ensures we replay the
// stored payload, not the upstream response).
func TestChat_SemanticHit_BodyMatchesStoredPayload(t *testing.T) {
	f := newSemanticChatFixture(t)

	_ = f.post(t, chatBody("test-model", "warm-up", false))
	rec := f.post(t, chatBody("test-model", "completely-different-text", false))
	require.Equal(t, http.StatusOK, rec.Code)
	require.Equal(t, "semantic", rec.Header().Get("X-X-Beacon-Cache-Layer"))

	var resp map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	// Mock upstream returns id=upstream-1; the cached payload (built
	// from that response) should round-trip identically.
	assert.Equal(t, "upstream-1", resp["id"])
}

// TestChat_SemanticMetricsScrape verifies the Week 10.7 metrics fire
// end-to-end: similarity histogram, semantic lookup duration, hit/
// write counters all increment after a miss + write + hit cycle.
func TestChat_SemanticMetricsScrape(t *testing.T) {
	upstream := func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{
			"id":"u","object":"chat.completion","created":1,"model":"test-model",
			"choices":[{"index":0,"message":{"role":"assistant","content":"hi"},"finish_reason":"stop"}],
			"usage":{"prompt_tokens":3,"completion_tokens":1,"total_tokens":4}
		}`))
	}
	reg, _ := buildRegistry(t, upstream)

	mr := miniredis.RunT(t)
	client := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = client.Close() })
	exact := cache.NewRedisExact(client)

	embedder := &stubEmbedder{vec: []float32{1, 0, 0, 0}}
	idx := &stubIndex{dim: 4}
	sc, err := cache.NewSemanticCache(cache.SemanticConfig{
		Embedder: embedder, Index: idx, Threshold: 0.95, TopK: 5,
	})
	require.NoError(t, err)

	metricsReg := prometheus.NewRegistry()
	gm, err := observability.NewMetrics(metricsReg)
	require.NoError(t, err)
	gm.SetSemanticThreshold(0.95)

	srv := newTestServer(t, func(d *Deps) {
		d.Registry = reg
		d.Router = newTestRouter(reg)
		d.Cache = exact
		d.CacheTTL = time.Minute
		d.Semantic = sc
		d.Metrics = gm
		d.MetricsReg = metricsReg
	})

	post := func(t *testing.T, body []byte) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", bytes.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		rec := httptest.NewRecorder()
		srv.Handler().ServeHTTP(rec, req)
		return rec
	}
	require.Equal(t, http.StatusOK, post(t, chatBody("test-model", "first", false)).Code)
	require.Equal(t, http.StatusOK, post(t, chatBody("test-model", "second-different", false)).Code)

	body := scrapeMetrics(t, srv)
	assert.Contains(t, body, `gateway_cache_semantic_threshold 0.95`)
	assert.Contains(t, body, `gateway_cache_writes_total{type="semantic"}`)
	assert.Contains(t, body, `gateway_cache_hits_total{type="semantic"} 1`)
	assert.Contains(t, body, `gateway_cache_semantic_lookup_duration_seconds_count{result="miss"}`)
	assert.Contains(t, body, `gateway_cache_semantic_lookup_duration_seconds_count{result="hit"} 1`)
	assert.Contains(t, body, `gateway_cache_semantic_similarity_count`)
}

// System-only request (no user message) flattens to "" → semantic
// Lookup is a no-op and falls through to upstream. Asserts that the
// semantic path doesn't choke on empty queries.
func TestChat_SemanticPath_SystemOnlyFlattenIsNoop(t *testing.T) {
	f := newSemanticChatFixture(t)

	body, _ := json.Marshal(map[string]any{
		"model":    "test-model",
		"messages": []map[string]any{{"role": "system", "content": "be helpful"}},
	})
	rec := f.post(t, body)
	require.Equal(t, http.StatusOK, rec.Code)
	assert.Equal(t, "miss", rec.Header().Get("X-X-Beacon-Cache"))
	assert.Equal(t, int64(1), f.upstream.Load())
}
