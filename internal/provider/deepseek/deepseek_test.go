package deepseek

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/An-idd/x-beacon/internal/provider"
	"github.com/An-idd/x-beacon/internal/provider/openai"
)

// newTestServer spins up an httptest server simulating DeepSeek and
// returns a Provider pointing at it. Caller supplies the handler.
func newTestServer(t *testing.T, handler http.HandlerFunc) *openai.Provider {
	t.Helper()
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)

	p, err := New(Config{
		Name:     "deepseek-test",
		Endpoint: srv.URL,
		APIKey:   "sk-test",
		Models: openai.Models{
			Exact: []string{"deepseek-chat", "deepseek-reasoner"},
		},
	})
	require.NoError(t, err)
	return p
}

func TestNew_RequiresAPIKey(t *testing.T) {
	_, err := New(Config{Name: "deepseek"})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "APIKey")
}

func TestNew_RequiresName(t *testing.T) {
	_, err := New(Config{APIKey: "sk-x"})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "Name")
}

func TestNew_ProviderNameCarried(t *testing.T) {
	p, err := New(Config{Name: "deepseek-primary", APIKey: "sk-x"})
	require.NoError(t, err)
	assert.Equal(t, "deepseek-primary", p.Name())
}

func TestNew_SupportedModelsFromConfig(t *testing.T) {
	p, err := New(Config{
		Name:   "deepseek",
		APIKey: "sk-x",
		Models: openai.Models{Exact: []string{"deepseek-chat", "deepseek-reasoner"}},
	})
	require.NoError(t, err)
	models := p.SupportedModels()
	require.Len(t, models, 2)
	assert.Equal(t, "deepseek-chat", models[0].ID)
	assert.Equal(t, "deepseek", models[0].OwnedBy) // owned_by is "openai" in the adapter; DeepSeek reuses it
}

// TestNonStreaming_HappyPath verifies the full request/response round-trip
// goes through the DeepSeek-configured endpoint and parses correctly.
func TestNonStreaming_HappyPath(t *testing.T) {
	var receivedAuth string
	p := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		receivedAuth = r.Header.Get("Authorization")
		_, _ = io.WriteString(w, `{
			"id": "deepseek-123",
			"object": "chat.completion",
			"created": 1700000000,
			"model": "deepseek-chat",
			"choices": [{
				"index": 0,
				"message": {"role":"assistant","content":"hello"},
				"finish_reason":"stop"
			}],
			"usage": {"prompt_tokens":3,"completion_tokens":1,"total_tokens":4}
		}`)
	})

	resp, err := p.ChatCompletion(context.Background(), &provider.ChatRequest{
		Model:    "deepseek-chat",
		Messages: []provider.Message{{Role: "user", Content: "hi"}},
	})
	require.NoError(t, err)
	assert.Equal(t, "Bearer sk-test", receivedAuth)
	assert.Equal(t, "deepseek-test", resp.Provider) // cfg.Name attribution
	assert.Equal(t, "hello", resp.Choices[0].Message.Content)
	assert.Equal(t, 4, resp.Usage.TotalTokens)
}

// TestStreaming_WithDoneMarker validates the Week 1 assumption: DeepSeek,
// being OpenAI-compatible, sends `data: [DONE]` to terminate the stream.
// If DeepSeek ever drops [DONE], this test can be adjusted and stream.go
// will need per-provider tolerance.
func TestStreaming_WithDoneMarker(t *testing.T) {
	p := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, "text/event-stream", r.Header.Get("Accept"))

		w.Header().Set("Content-Type", "text/event-stream")
		flusher, _ := w.(http.Flusher)
		frames := []string{
			`data: {"id":"ds-1","object":"chat.completion.chunk","created":1,"model":"deepseek-chat","choices":[{"index":0,"delta":{"role":"assistant","content":""}}]}`,
			`data: {"id":"ds-1","object":"chat.completion.chunk","created":1,"model":"deepseek-chat","choices":[{"index":0,"delta":{"content":"Hel"}}]}`,
			`data: {"id":"ds-1","object":"chat.completion.chunk","created":1,"model":"deepseek-chat","choices":[{"index":0,"delta":{"content":"lo"},"finish_reason":"stop"}]}`,
			`data: [DONE]`,
		}
		_, _ = io.WriteString(w, strings.Join(frames, "\n\n")+"\n\n")
		if flusher != nil {
			flusher.Flush()
		}
	})

	ch, err := p.ChatCompletionStream(context.Background(), &provider.ChatRequest{
		Model:    "deepseek-chat",
		Messages: []provider.Message{{Role: "user", Content: "hi"}},
	})
	require.NoError(t, err)

	var chunks int
	for ev := range ch {
		require.NoError(t, ev.Err, "happy path must not emit error events")
		chunks++
	}
	assert.Equal(t, 3, chunks)
}

// TestStreaming_WithoutDoneMarker_EmitsError documents the current
// behavior when a stream terminates cleanly (EOF) without [DONE]. This is
// treated as truncation. If real DeepSeek ever lands here, we'll need to
// make the [DONE] expectation per-provider configurable.
func TestStreaming_WithoutDoneMarker_EmitsError(t *testing.T) {
	p := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		flusher, _ := w.(http.Flusher)
		_, _ = io.WriteString(w, `data: {"id":"ds-1","object":"chat.completion.chunk","created":1,"model":"deepseek-chat","choices":[{"index":0,"delta":{"content":"hi"},"finish_reason":"stop"}]}`+"\n\n")
		if flusher != nil {
			flusher.Flush()
		}
		// handler returns without emitting [DONE]
	})

	ch, err := p.ChatCompletionStream(context.Background(), &provider.ChatRequest{
		Model:    "deepseek-chat",
		Messages: []provider.Message{{Role: "user", Content: "hi"}},
	})
	require.NoError(t, err)

	var chunks int
	var lastErr error
	for ev := range ch {
		if ev.Err != nil {
			lastErr = ev.Err
			continue
		}
		chunks++
	}
	assert.Equal(t, 1, chunks)
	require.Error(t, lastErr)
	assert.True(t, errors.Is(lastErr, provider.ErrUpstream))
	assert.Contains(t, lastErr.Error(), "[DONE]")
}

// TestErrorPassthrough_401 verifies DeepSeek's error responses (which
// follow OpenAI's error envelope) are mapped to the right sentinel.
func TestErrorPassthrough_401(t *testing.T) {
	p := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = io.WriteString(w, `{"error":{"message":"invalid key","type":"invalid_request_error","code":"invalid_api_key"}}`)
	})

	_, err := p.ChatCompletion(context.Background(), &provider.ChatRequest{
		Model:    "deepseek-chat",
		Messages: []provider.Message{{Role: "user", Content: "hi"}},
	})
	require.Error(t, err)
	assert.True(t, errors.Is(err, provider.ErrAuth))
}
