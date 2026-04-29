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
	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	"github.com/An-idd/x-beacon/internal/cache"
	"github.com/An-idd/x-beacon/internal/provider"
	"github.com/An-idd/x-beacon/internal/server/sse"
)

// parseSSEFrames splits a recorder body on the SSE frame boundary
// ("\n\n") and strips the "data: " prefix. Returns each payload
// (whether JSON or "[DONE]") in order.
func parseSSEFrames(t *testing.T, body string) []string {
	t.Helper()
	frames := []string{}
	for _, raw := range strings.Split(body, "\n\n") {
		raw = strings.TrimSpace(raw)
		if raw == "" {
			continue
		}
		if !strings.HasPrefix(raw, "data: ") {
			// Heartbeat comment lines etc. — ignore.
			continue
		}
		frames = append(frames, strings.TrimPrefix(raw, "data: "))
	}
	return frames
}

// flushRecorder is an httptest.ResponseRecorder that satisfies
// http.Flusher so sse.New() works in tests.
type flushRecorder struct {
	*httptest.ResponseRecorder
}

func (f *flushRecorder) Flush() {}

func newFlushRecorder() *flushRecorder {
	return &flushRecorder{ResponseRecorder: httptest.NewRecorder()}
}

func TestReplayCachedStream_EmitsRoleContentFinishDone(t *testing.T) {
	rec := newFlushRecorder()
	sw, err := sse.New(rec)
	require.NoError(t, err)

	cached := &provider.ChatResponse{
		ID: "chatcmpl-cached", Object: "chat.completion", Created: 1714000000,
		Model: "test-model",
		Choices: []provider.Choice{{
			Index:        0,
			Message:      provider.Message{Role: "assistant", Content: "hello world"},
			FinishReason: "stop",
		}},
		Usage: &provider.Usage{PromptTokens: 3, CompletionTokens: 2, TotalTokens: 5},
	}

	replayCachedStream(sw, cached, "req-test", zap.NewNop())
	frames := parseSSEFrames(t, rec.Body.String())

	// Expect: 1 role frame + 1 content frame (11 chars < 32) + 1 finish frame + [DONE]
	require.Equal(t, 4, len(frames), "frames=%v", frames)

	// Frame 1: role only.
	var f1 provider.ChatStreamChunk
	require.NoError(t, json.Unmarshal([]byte(frames[0]), &f1))
	require.Len(t, f1.Choices, 1)
	assert.Equal(t, "assistant", f1.Choices[0].Delta.Role)
	assert.Empty(t, f1.Choices[0].Delta.Content)

	// Frame 2: content.
	var f2 provider.ChatStreamChunk
	require.NoError(t, json.Unmarshal([]byte(frames[1]), &f2))
	require.Len(t, f2.Choices, 1)
	assert.Equal(t, "hello world", f2.Choices[0].Delta.Content)
	assert.Empty(t, f2.Choices[0].Delta.Role)
	assert.Empty(t, f2.Choices[0].FinishReason)

	// Frame 3: finish + cached usage.
	var f3 provider.ChatStreamChunk
	require.NoError(t, json.Unmarshal([]byte(frames[2]), &f3))
	require.Len(t, f3.Choices, 1)
	assert.Equal(t, "stop", f3.Choices[0].FinishReason)
	require.NotNil(t, f3.Usage, "usage must be replayed on terminal chunk")
	assert.Equal(t, 5, f3.Usage.TotalTokens)

	// Frame 4: [DONE] sentinel.
	assert.Equal(t, "[DONE]", frames[3])
}

func TestReplayCachedStream_StableIdAcrossFrames(t *testing.T) {
	rec := newFlushRecorder()
	sw, err := sse.New(rec)
	require.NoError(t, err)

	cached := &provider.ChatResponse{
		ID:    "chatcmpl-stable",
		Model: "x",
		Choices: []provider.Choice{{
			Index:        0,
			Message:      provider.Message{Role: "assistant", Content: "abc"},
			FinishReason: "stop",
		}},
	}
	replayCachedStream(sw, cached, "req", zap.NewNop())
	frames := parseSSEFrames(t, rec.Body.String())

	require.GreaterOrEqual(t, len(frames), 3)
	for _, f := range frames[:len(frames)-1] { // skip [DONE]
		var c provider.ChatStreamChunk
		require.NoError(t, json.Unmarshal([]byte(f), &c))
		assert.Equal(t, "chatcmpl-stable", c.ID, "all chunks must share the cached response id")
	}
}

func TestReplayCachedStream_LongContentIsChunkedByRune(t *testing.T) {
	rec := newFlushRecorder()
	sw, err := sse.New(rec)
	require.NoError(t, err)

	// 100 runes of mixed ASCII + multibyte CJK to exercise rune-safe slicing.
	content := strings.Repeat("a你b", 34) // 102 runes total
	cached := &provider.ChatResponse{
		ID:    "x",
		Model: "m",
		Choices: []provider.Choice{{
			Index:        0,
			Message:      provider.Message{Role: "assistant", Content: content},
			FinishReason: "stop",
		}},
	}
	replayCachedStream(sw, cached, "req", zap.NewNop())
	frames := parseSSEFrames(t, rec.Body.String())

	// Reassemble content from delta frames and verify it round-trips.
	var got strings.Builder
	for _, f := range frames {
		if f == "[DONE]" {
			continue
		}
		var c provider.ChatStreamChunk
		require.NoError(t, json.Unmarshal([]byte(f), &c))
		for _, ch := range c.Choices {
			got.WriteString(ch.Delta.Content)
		}
	}
	assert.Equal(t, content, got.String(), "round-trip must preserve every codepoint")

	// Sanity: at least one content frame was emitted (102 runes / 32 = 4 frames).
	contentFrames := 0
	for _, f := range frames {
		if f == "[DONE]" {
			continue
		}
		var c provider.ChatStreamChunk
		_ = json.Unmarshal([]byte(f), &c)
		if len(c.Choices) > 0 && c.Choices[0].Delta.Content != "" {
			contentFrames++
		}
	}
	assert.GreaterOrEqual(t, contentFrames, 4)
}

func TestReplayCachedStream_EmptyContentStillEmitsRoleAndFinish(t *testing.T) {
	// Cached responses with empty content shouldn't reach Set thanks
	// to the anti-pollution gate, but if they do the replay must
	// still produce a syntactically valid stream (role + finish + DONE).
	rec := newFlushRecorder()
	sw, err := sse.New(rec)
	require.NoError(t, err)

	cached := &provider.ChatResponse{
		ID: "x", Model: "m",
		Choices: []provider.Choice{{
			Index:        0,
			Message:      provider.Message{Role: "assistant", Content: ""},
			FinishReason: "stop",
		}},
	}
	replayCachedStream(sw, cached, "req", zap.NewNop())
	frames := parseSSEFrames(t, rec.Body.String())

	require.GreaterOrEqual(t, len(frames), 3)
	assert.Equal(t, "[DONE]", frames[len(frames)-1])
}

func TestReplayCachedStream_MultiChoiceReplaysAllInOrder(t *testing.T) {
	rec := newFlushRecorder()
	sw, err := sse.New(rec)
	require.NoError(t, err)

	cached := &provider.ChatResponse{
		ID: "x", Model: "m",
		Choices: []provider.Choice{
			{Index: 0, Message: provider.Message{Role: "assistant", Content: "alpha"}, FinishReason: "stop"},
			{Index: 1, Message: provider.Message{Role: "assistant", Content: "beta"}, FinishReason: "stop"},
		},
	}
	replayCachedStream(sw, cached, "req", zap.NewNop())
	frames := parseSSEFrames(t, rec.Body.String())

	// Find the content frames for each index and verify ordering.
	var seen0, seen1 strings.Builder
	for _, f := range frames {
		if f == "[DONE]" {
			continue
		}
		var c provider.ChatStreamChunk
		_ = json.Unmarshal([]byte(f), &c)
		for _, ch := range c.Choices {
			switch ch.Index {
			case 0:
				seen0.WriteString(ch.Delta.Content)
			case 1:
				seen1.WriteString(ch.Delta.Content)
			}
		}
	}
	assert.Equal(t, "alpha", seen0.String())
	assert.Equal(t, "beta", seen1.String())
}

// End-to-end: streaming request hits cache and gets the replayed
// frames with X-X-Beacon-Cache: hit header. Upstream must NOT be
// reached.
func TestChat_StreamCacheHit_ReplaysWithoutUpstream(t *testing.T) {
	upstreamHits := &atomic.Int64{}
	upstream := func(w http.ResponseWriter, _ *http.Request) {
		upstreamHits.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"u","object":"chat.completion","model":"test-model","choices":[{"index":0,"message":{"role":"assistant","content":"upstream"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`))
	}
	reg, _ := buildRegistry(t, upstream)

	mr := miniredis.RunT(t)
	client := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = client.Close() })
	exact := cache.NewRedisExact(client)

	// Pre-seed cache for the (model, "ping") request.
	seedReq := &provider.ChatRequest{
		Model:    "test-model",
		Messages: []provider.Message{{Role: "user", Content: "ping"}},
	}
	key, err := cache.Key(seedReq)
	require.NoError(t, err)
	cached := &provider.ChatResponse{
		ID: "chatcmpl-cached", Object: "chat.completion", Created: 1, Model: "test-model",
		Choices: []provider.Choice{{
			Index:        0,
			Message:      provider.Message{Role: "assistant", Content: "from-cache"},
			FinishReason: "stop",
		}},
		Usage: &provider.Usage{PromptTokens: 1, CompletionTokens: 1, TotalTokens: 2},
	}
	require.NoError(t, exact.Set(t.Context(), key, cached, time.Minute))

	srv := newTestServer(t, func(d *Deps) {
		d.Registry = reg
		d.Router = newTestRouter(reg)
		d.Cache = exact
		d.CacheTTL = time.Minute
	})

	body := chatBody("test-model", "ping", true)
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rec := newFlushRecorder()
	srv.Handler().ServeHTTP(rec, req)

	require.Equal(t, http.StatusOK, rec.Code)
	assert.Equal(t, "hit", rec.Header().Get("X-X-Beacon-Cache"))
	assert.Equal(t, int64(0), upstreamHits.Load(), "stream hit must not call upstream")

	frames := parseSSEFrames(t, rec.Body.String())
	require.NotEmpty(t, frames)
	assert.Equal(t, "[DONE]", frames[len(frames)-1])

	// At least one content frame must contain the cached content.
	var assembled strings.Builder
	for _, f := range frames {
		if f == "[DONE]" {
			continue
		}
		var c provider.ChatStreamChunk
		_ = json.Unmarshal([]byte(f), &c)
		for _, ch := range c.Choices {
			assembled.WriteString(ch.Delta.Content)
		}
	}
	assert.Equal(t, "from-cache", assembled.String())
}

// TestChat_StreamMissThenStreamHit: a streaming miss writes the
// aggregated response to cache; a follow-up stream with identical
// inputs hits and replays without contacting upstream.
//
// This proves the round-trip: stream → upstream → write-back →
// stream → cache hit → replay.
func TestChat_StreamMissThenStreamHit(t *testing.T) {
	upstreamHits := &atomic.Int64{}
	// Real-shaped SSE upstream so the gateway's stream parser produces
	// observable streamStats and the write-back gate (finish_reason=stop)
	// is satisfied.
	upstream := func(w http.ResponseWriter, _ *http.Request) {
		upstreamHits.Add(1)
		w.Header().Set("Content-Type", "text/event-stream")
		f, ok := w.(http.Flusher)
		if !ok {
			t.Fatalf("upstream writer is not a Flusher")
		}
		// Two content frames + a finish frame + [DONE]. Per OpenAI wire
		// shape: each `data: ` line followed by a blank line.
		writeFrame := func(payload string) {
			_, _ = w.Write([]byte("data: " + payload + "\n\n"))
			f.Flush()
		}
		writeFrame(`{"id":"u1","object":"chat.completion.chunk","created":1,"model":"test-model","choices":[{"index":0,"delta":{"role":"assistant"}}]}`)
		writeFrame(`{"id":"u1","object":"chat.completion.chunk","created":1,"model":"test-model","choices":[{"index":0,"delta":{"content":"hello "}}]}`)
		writeFrame(`{"id":"u1","object":"chat.completion.chunk","created":1,"model":"test-model","choices":[{"index":0,"delta":{"content":"world"}}]}`)
		writeFrame(`{"id":"u1","object":"chat.completion.chunk","created":1,"model":"test-model","choices":[{"index":0,"delta":{},"finish_reason":"stop"}],"usage":{"prompt_tokens":3,"completion_tokens":2,"total_tokens":5}}`)
		writeFrame("[DONE]")
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

	post := func() *flushRecorder {
		req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions",
			bytes.NewReader(chatBody("test-model", "round-trip", true)))
		req.Header.Set("Content-Type", "application/json")
		rec := newFlushRecorder()
		srv.Handler().ServeHTTP(rec, req)
		return rec
	}

	// First stream: miss + write-back.
	rec1 := post()
	require.Equal(t, http.StatusOK, rec1.Code)
	assert.Equal(t, "miss", rec1.Header().Get("X-X-Beacon-Cache"))
	assert.Equal(t, int64(1), upstreamHits.Load())

	// Second stream: hit, no upstream call.
	rec2 := post()
	require.Equal(t, http.StatusOK, rec2.Code)
	assert.Equal(t, "hit", rec2.Header().Get("X-X-Beacon-Cache"))
	assert.Equal(t, int64(1), upstreamHits.Load(), "stream hit must not call upstream")

	// Replay must reconstruct the original content.
	frames := parseSSEFrames(t, rec2.Body.String())
	require.NotEmpty(t, frames)
	var content strings.Builder
	for _, f := range frames {
		if f == "[DONE]" {
			continue
		}
		var c provider.ChatStreamChunk
		_ = json.Unmarshal([]byte(f), &c)
		for _, ch := range c.Choices {
			content.WriteString(ch.Delta.Content)
		}
	}
	assert.Equal(t, "hello world", content.String())
}

// Streaming miss must transition cleanly to the upstream path with
// X-X-Beacon-Cache: miss header.
func TestChat_StreamCacheMiss_FallsThroughToUpstream(t *testing.T) {
	upstreamHits := &atomic.Int64{}
	upstream := func(w http.ResponseWriter, _ *http.Request) {
		upstreamHits.Add(1)
		// Mock upstream returns non-SSE; we only care about the header
		// and the upstream-call count, not the resulting stream.
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"u","object":"chat.completion","model":"test-model","choices":[]}`))
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

	body := chatBody("test-model", "cold-key", true)
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rec := newFlushRecorder()
	srv.Handler().ServeHTTP(rec, req)

	assert.Equal(t, "miss", rec.Header().Get("X-X-Beacon-Cache"))
	assert.GreaterOrEqual(t, upstreamHits.Load(), int64(1))
}
