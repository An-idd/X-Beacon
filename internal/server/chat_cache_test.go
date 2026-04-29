package server

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/An-idd/x-beacon/internal/cache"
	"github.com/An-idd/x-beacon/internal/observability"
	"github.com/An-idd/x-beacon/internal/provider"
)

// chatCacheFixture wires a chat handler with an in-memory exact cache
// and an upstream call counter so tests can prove "cache hit → upstream
// not called".
type chatCacheFixture struct {
	srv      *Server
	cache    cache.Exact
	upstream *atomic.Int64
}

func newChatCacheFixture(t *testing.T) *chatCacheFixture {
	t.Helper()
	upstreamHits := &atomic.Int64{}
	upstream := func(w http.ResponseWriter, _ *http.Request) {
		upstreamHits.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{
			"id":"chatcmpl-1","object":"chat.completion","created":1714000000,"model":"test-model",
			"choices":[{"index":0,"message":{"role":"assistant","content":"hi"},"finish_reason":"stop"}],
			"usage":{"prompt_tokens":3,"completion_tokens":1,"total_tokens":4}
		}`))
	}
	reg, _ := buildRegistry(t, upstream)

	mr := miniredis.RunT(t)
	client := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = client.Close() })
	exact := cache.NewRedisExact(client)

	srv := newTestServer(t, func(d *Deps) {
		d.Registry = reg
		d.Router = newTestRouter(reg)
		d.Cache = exact
		d.CacheTTL = time.Minute
	})
	return &chatCacheFixture{srv: srv, cache: exact, upstream: upstreamHits}
}

func (f *chatCacheFixture) post(t *testing.T, body []byte) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	f.srv.Handler().ServeHTTP(rec, req)
	return rec
}

func TestChatCache_MissAnnotatesHeaderAndCallsUpstream(t *testing.T) {
	f := newChatCacheFixture(t)

	rec := f.post(t, chatBody("test-model", "hello", false))
	require.Equal(t, http.StatusOK, rec.Code)
	assert.Equal(t, "miss", rec.Header().Get("X-X-Beacon-Cache"))
	assert.Equal(t, int64(1), f.upstream.Load())
}

func TestChatCache_HitShortCircuitsUpstream(t *testing.T) {
	f := newChatCacheFixture(t)

	// Pre-seed the cache so the first request is already a hit.
	req := &provider.ChatRequest{
		Model:    "test-model",
		Messages: []provider.Message{{Role: "user", Content: "hello"}},
	}
	key, err := cache.Key(req)
	require.NoError(t, err)
	cached := &provider.ChatResponse{
		ID: "chatcmpl-cached", Object: "chat.completion", Model: "test-model",
		Choices: []provider.Choice{{Index: 0, Message: provider.Message{Role: "assistant", Content: "from-cache"}, FinishReason: "stop"}},
		Usage:   &provider.Usage{PromptTokens: 3, CompletionTokens: 1, TotalTokens: 4},
	}
	require.NoError(t, f.cache.Set(t.Context(), key, cached, time.Minute))

	rec := f.post(t, chatBody("test-model", "hello", false))
	require.Equal(t, http.StatusOK, rec.Code)
	assert.Equal(t, "hit", rec.Header().Get("X-X-Beacon-Cache"))
	assert.Equal(t, int64(0), f.upstream.Load(), "upstream must not be called on cache hit")

	var resp map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	assert.Equal(t, "chatcmpl-cached", resp["id"])
}

func TestChatCache_StreamRequestIsBypassed(t *testing.T) {
	// Streaming bypasses the cache (Decision 4). The default fixture
	// upstream replies with non-SSE JSON so the streaming router will
	// surface a 502; we don't care about the streaming wire format here,
	// only that the bypass header was emitted before the stream branch
	// took over and that the upstream was contacted.
	f := newChatCacheFixture(t)

	rec := f.post(t, chatBody("test-model", "hello", true))
	assert.Equal(t, "bypass", rec.Header().Get("X-X-Beacon-Cache"))
	assert.GreaterOrEqual(t, f.upstream.Load(), int64(1))
}

func TestChatCache_DiscriminatesByMessageContent(t *testing.T) {
	// Two requests with different bodies must not share the cache.
	f := newChatCacheFixture(t)

	rec1 := f.post(t, chatBody("test-model", "alpha", false))
	require.Equal(t, http.StatusOK, rec1.Code)
	assert.Equal(t, "miss", rec1.Header().Get("X-X-Beacon-Cache"))

	rec2 := f.post(t, chatBody("test-model", "beta", false))
	require.Equal(t, http.StatusOK, rec2.Code)
	assert.Equal(t, "miss", rec2.Header().Get("X-X-Beacon-Cache"),
		"different message content must produce a different key")
	assert.Equal(t, int64(2), f.upstream.Load())
}

func TestChatCache_NilCache_NoHeaderEmitted(t *testing.T) {
	// When Cache is not wired, the handler must not emit the X-X-Beacon-Cache
	// header at all (clients shouldn't see misleading "miss" when caching
	// is disabled).
	upstream := func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"x","object":"chat.completion","model":"test-model","choices":[{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`))
	}
	srv := newChatHandlerSrv(t, upstream) // no Cache opt

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", bytes.NewReader(chatBody("test-model", "hi", false)))
	req.Header.Set("Content-Type", "application/json")
	srv.Handler().ServeHTTP(rec, req)
	require.Equal(t, http.StatusOK, rec.Code)
	assert.Empty(t, rec.Header().Get("X-X-Beacon-Cache"))
}

// TestChatCache_BackendErrorTreatedAsMiss verifies that a cache backend
// failure (e.g. Redis down) does not break the chat hot path: the
// handler logs and falls through to the upstream.
// TestChatCache_WriteBackOnSuccess verifies the happy-path write hook:
// after a miss + successful upstream, a follow-up identical request
// must hit cache and skip the upstream entirely.
func TestChatCache_WriteBackOnSuccess(t *testing.T) {
	f := newChatCacheFixture(t)

	// First request: miss + writes to cache.
	rec1 := f.post(t, chatBody("test-model", "hello", false))
	require.Equal(t, http.StatusOK, rec1.Code)
	require.Equal(t, "miss", rec1.Header().Get("X-X-Beacon-Cache"))
	require.Equal(t, int64(1), f.upstream.Load())

	// Second identical request: must be a hit.
	rec2 := f.post(t, chatBody("test-model", "hello", false))
	require.Equal(t, http.StatusOK, rec2.Code)
	assert.Equal(t, "hit", rec2.Header().Get("X-X-Beacon-Cache"))
	assert.Equal(t, int64(1), f.upstream.Load(), "second call must come from cache, not upstream")
}

func TestShouldCacheResponse_AntiPollutionGate(t *testing.T) {
	good := &provider.ChatResponse{
		Choices: []provider.Choice{{
			Index:        0,
			Message:      provider.Message{Role: "assistant", Content: "ok"},
			FinishReason: "stop",
		}},
	}

	cases := []struct {
		name     string
		resp     *provider.ChatResponse
		promptTok int
		want     bool
	}{
		{"happy path", good, 5, true},
		{"nil response", nil, 5, false},
		{"prompt tokens zero", good, 0, false},
		{"prompt tokens negative", good, -1, false},
		{"no choices", &provider.ChatResponse{}, 5, false},
		{"finish_reason length", &provider.ChatResponse{
			Choices: []provider.Choice{{Message: provider.Message{Content: "ok"}, FinishReason: "length"}},
		}, 5, false},
		{"finish_reason content_filter", &provider.ChatResponse{
			Choices: []provider.Choice{{Message: provider.Message{Content: "ok"}, FinishReason: "content_filter"}},
		}, 5, false},
		{"finish_reason tool_calls", &provider.ChatResponse{
			Choices: []provider.Choice{{Message: provider.Message{Content: "ok"}, FinishReason: "tool_calls"}},
		}, 5, false},
		{"empty content", &provider.ChatResponse{
			Choices: []provider.Choice{{Message: provider.Message{Content: ""}, FinishReason: "stop"}},
		}, 5, false},
		{"second choice has content (n>1)", &provider.ChatResponse{
			Choices: []provider.Choice{
				{Message: provider.Message{Content: ""}, FinishReason: "stop"},
				{Message: provider.Message{Content: "ok"}, FinishReason: "stop"},
			},
		}, 5, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, shouldCacheResponse(tc.resp, tc.promptTok))
		})
	}
}

// TestChatCache_TruncatedResponseNotCached: upstream returns
// finish_reason=length; the response must reach the client but the
// cache must NOT remember it (Decision 3).
func TestChatCache_TruncatedResponseNotCached(t *testing.T) {
	upstreamHits := &atomic.Int64{}
	upstream := func(w http.ResponseWriter, _ *http.Request) {
		upstreamHits.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{
			"id":"chatcmpl-trunc","object":"chat.completion","created":1714000000,"model":"test-model",
			"choices":[{"index":0,"message":{"role":"assistant","content":"half"},"finish_reason":"length"}],
			"usage":{"prompt_tokens":3,"completion_tokens":1,"total_tokens":4}
		}`))
	}
	reg, _ := buildRegistry(t, upstream)

	mr := miniredis.RunT(t)
	client := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = client.Close() })
	exact := cache.NewRedisExact(client)

	srv := newTestServer(t, func(d *Deps) {
		d.Registry = reg
		d.Router = newTestRouter(reg)
		d.Cache = exact
		d.CacheTTL = time.Minute
	})

	post := func() *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions",
			bytes.NewReader(chatBody("test-model", "hello", false)))
		req.Header.Set("Content-Type", "application/json")
		rec := httptest.NewRecorder()
		srv.Handler().ServeHTTP(rec, req)
		return rec
	}

	rec1 := post()
	require.Equal(t, http.StatusOK, rec1.Code)
	rec2 := post()
	require.Equal(t, http.StatusOK, rec2.Code)
	// Both hit upstream — the truncated response was never cached.
	assert.Equal(t, "miss", rec2.Header().Get("X-X-Beacon-Cache"))
	assert.Equal(t, int64(2), upstreamHits.Load())
}

// TestChatCache_ZeroTTLDisablesWrites: CacheTTL=0 must disable the
// write hook (reads still no-op via nil cache, but with cache present
// + TTL zero we read but never write).
func TestChatCache_ZeroTTLDisablesWrites(t *testing.T) {
	upstreamHits := &atomic.Int64{}
	upstream := func(w http.ResponseWriter, _ *http.Request) {
		upstreamHits.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{
			"id":"chatcmpl-1","object":"chat.completion","created":1714000000,"model":"test-model",
			"choices":[{"index":0,"message":{"role":"assistant","content":"hi"},"finish_reason":"stop"}],
			"usage":{"prompt_tokens":3,"completion_tokens":1,"total_tokens":4}
		}`))
	}
	reg, _ := buildRegistry(t, upstream)

	mr := miniredis.RunT(t)
	client := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = client.Close() })
	exact := cache.NewRedisExact(client)

	srv := newTestServer(t, func(d *Deps) {
		d.Registry = reg
		d.Router = newTestRouter(reg)
		d.Cache = exact
		d.CacheTTL = 0 // writes disabled
	})

	post := func() *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions",
			bytes.NewReader(chatBody("test-model", "hello", false)))
		req.Header.Set("Content-Type", "application/json")
		rec := httptest.NewRecorder()
		srv.Handler().ServeHTTP(rec, req)
		return rec
	}

	require.Equal(t, http.StatusOK, post().Code)
	rec2 := post()
	require.Equal(t, http.StatusOK, rec2.Code)
	assert.Equal(t, "miss", rec2.Header().Get("X-X-Beacon-Cache"))
	assert.Equal(t, int64(2), upstreamHits.Load())
}

// TestChatCache_MetricsFireOnHitAndWrite verifies the Week 9 metric
// surface: a miss + write produces gateway_cache_writes_total{type="exact"}=1
// and gateway_cache_lookup_duration_seconds{result="miss"} samples; a
// follow-up hit produces gateway_cache_hits_total{type="exact"}=1 plus a
// {result="hit"} lookup observation.
func TestChatCache_MetricsFireOnHitAndWrite(t *testing.T) {
	upstream := func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{
			"id":"chatcmpl-1","object":"chat.completion","created":1714000000,"model":"test-model",
			"choices":[{"index":0,"message":{"role":"assistant","content":"hi"},"finish_reason":"stop"}],
			"usage":{"prompt_tokens":3,"completion_tokens":1,"total_tokens":4}
		}`))
	}
	reg, _ := buildRegistry(t, upstream)

	mr := miniredis.RunT(t)
	client := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = client.Close() })
	exact := cache.NewRedisExact(client)

	metricsReg := prometheus.NewRegistry()
	gm, err := observability.NewMetrics(metricsReg)
	require.NoError(t, err)

	srv := newTestServer(t, func(d *Deps) {
		d.Registry = reg
		d.Router = newTestRouter(reg)
		d.Cache = exact
		d.CacheTTL = time.Minute
		d.Metrics = gm
		d.MetricsReg = metricsReg
	})

	post := func() {
		req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions",
			bytes.NewReader(chatBody("test-model", "hello", false)))
		req.Header.Set("Content-Type", "application/json")
		rec := httptest.NewRecorder()
		srv.Handler().ServeHTTP(rec, req)
		require.Equal(t, http.StatusOK, rec.Code)
	}

	post() // miss + write
	post() // hit

	scrape := scrapeMetrics(t, srv)
	assert.Contains(t, scrape, `gateway_cache_writes_total{type="exact"} 1`)
	assert.Contains(t, scrape, `gateway_cache_hits_total{type="exact"} 1`)
	// Histograms emit *_count series; one observation per outcome.
	assert.Contains(t, scrape, `gateway_cache_lookup_duration_seconds_count{result="miss"} 1`)
	assert.Contains(t, scrape, `gateway_cache_lookup_duration_seconds_count{result="hit"} 1`)
}

// scrapeMetrics fetches /metrics from the assembled server and returns
// it as a single string for substring assertions.
func scrapeMetrics(t *testing.T, srv *Server) string {
	t.Helper()
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	require.Equal(t, http.StatusOK, rec.Code)
	body := rec.Body.String()
	// Sanity check: this should always be present when /metrics works.
	require.True(t, strings.Contains(body, "gateway_requests_total"))
	return body
}

func TestChatCache_BackendErrorTreatedAsMiss(t *testing.T) {
	upstreamHits := &atomic.Int64{}
	upstream := func(w http.ResponseWriter, _ *http.Request) {
		upstreamHits.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"x","object":"chat.completion","model":"test-model","choices":[{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`))
	}
	reg, _ := buildRegistry(t, upstream)

	// Point at a closed redis client so every Get returns an error.
	mr := miniredis.RunT(t)
	client := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	require.NoError(t, client.Close())
	exact := cache.NewRedisExact(client)

	srv := newTestServer(t, func(d *Deps) {
		d.Registry = reg
		d.Router = newTestRouter(reg)
		d.Cache = exact
		d.CacheTTL = time.Minute
	})

	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions",
		bytes.NewReader(chatBody("test-model", "hello", false)))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)

	require.Equal(t, http.StatusOK, rec.Code)
	assert.Equal(t, "miss", rec.Header().Get("X-X-Beacon-Cache"),
		"backend error must surface as miss, not a 5xx")
	assert.Equal(t, int64(1), upstreamHits.Load())
}

