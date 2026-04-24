package openai

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/An-idd/x-beacon/internal/provider"
)

// openaiSuccessBody is a real-shape /v1/chat/completions response taken from
// the OpenAI docs, abbreviated. Parsers must tolerate extra fields.
const openaiSuccessBody = `{
  "id": "chatcmpl-123",
  "object": "chat.completion",
  "created": 1700000000,
  "model": "gpt-4o-mini",
  "choices": [{
    "index": 0,
    "message": {"role":"assistant","content":"hello from mock"},
    "finish_reason":"stop"
  }],
  "usage": {"prompt_tokens":5,"completion_tokens":4,"total_tokens":9}
}`

func basicRequest() *provider.ChatRequest {
	return &provider.ChatRequest{
		Model:    "gpt-4o-mini",
		Messages: []provider.Message{{Role: "user", Content: "hi"}},
	}
}

func TestChatCompletion_Success(t *testing.T) {
	p := newTestProvider(t, func(w http.ResponseWriter, r *http.Request) {
		// Verify headers + path + body contract.
		assert.Equal(t, http.MethodPost, r.Method)
		assert.Equal(t, chatPath, r.URL.Path)
		assert.Equal(t, "Bearer sk-test", r.Header.Get("Authorization"))
		assert.Equal(t, "org-test", r.Header.Get("OpenAI-Organization"))

		var inbound provider.ChatRequest
		body, _ := io.ReadAll(r.Body)
		require.NoError(t, json.Unmarshal(body, &inbound))
		// stream=false must be enforced even if caller set true.
		assert.False(t, inbound.Stream)

		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, openaiSuccessBody)
	})

	req := basicRequest()
	req.Stream = true // should be overridden
	resp, err := p.ChatCompletion(context.Background(), req)
	require.NoError(t, err)
	require.NotNil(t, resp)
	assert.Equal(t, "chatcmpl-123", resp.ID)
	assert.Equal(t, "gpt-4o-mini", resp.Model)
	assert.Equal(t, "openai-test", resp.Provider)
	require.Len(t, resp.Choices, 1)
	assert.Equal(t, "hello from mock", resp.Choices[0].Message.Content)
	require.NotNil(t, resp.Usage)
	assert.Equal(t, 9, resp.Usage.TotalTokens)
}

func TestChatCompletion_Error_401_Auth(t *testing.T) {
	p := newTestProvider(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = io.WriteString(w, `{"error":{"message":"Invalid API key","type":"invalid_request_error","code":"invalid_api_key"}}`)
	})

	_, err := p.ChatCompletion(context.Background(), basicRequest())
	require.Error(t, err)
	assert.True(t, errors.Is(err, provider.ErrAuth))
	assert.False(t, provider.IsRetryable(err))
}

func TestChatCompletion_Error_429_RateLimit_WithRetryAfter(t *testing.T) {
	p := newTestProvider(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Retry-After", "30")
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = io.WriteString(w, `{"error":{"message":"Rate limit exceeded","type":"rate_limit_error","code":"rate_limit_exceeded"}}`)
	})

	_, err := p.ChatCompletion(context.Background(), basicRequest())
	require.Error(t, err)
	assert.True(t, errors.Is(err, provider.ErrRateLimited))
	assert.True(t, provider.IsRetryable(err))

	var ue *provider.UpstreamError
	require.True(t, errors.As(err, &ue))
	assert.Equal(t, 30*time.Second, ue.RetryAfter)
}

func TestChatCompletion_Error_400_ContextLength(t *testing.T) {
	p := newTestProvider(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = io.WriteString(w, `{"error":{"message":"maximum context length is 128000 tokens","type":"invalid_request_error","code":"context_length_exceeded"}}`)
	})

	_, err := p.ChatCompletion(context.Background(), basicRequest())
	require.Error(t, err)
	assert.True(t, errors.Is(err, provider.ErrContextLength))
	assert.False(t, provider.IsRetryable(err))
}

func TestChatCompletion_Error_400_Invalid(t *testing.T) {
	p := newTestProvider(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = io.WriteString(w, `{"error":{"message":"messages must be an array","type":"invalid_request_error","code":"invalid_request"}}`)
	})

	_, err := p.ChatCompletion(context.Background(), basicRequest())
	require.Error(t, err)
	assert.True(t, errors.Is(err, provider.ErrInvalidRequest))
	assert.False(t, errors.Is(err, provider.ErrContextLength))
}

func TestChatCompletion_Error_500_Upstream(t *testing.T) {
	p := newTestProvider(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = io.WriteString(w, `{"error":{"message":"server exploded"}}`)
	})

	_, err := p.ChatCompletion(context.Background(), basicRequest())
	require.Error(t, err)
	assert.True(t, errors.Is(err, provider.ErrUpstream))
	assert.True(t, provider.IsRetryable(err))
}

func TestChatCompletion_ContextCanceled(t *testing.T) {
	// Use a pre-cancelled ctx so the client never actually dispatches the
	// request; this avoids the httptest deadlock where Server.Close blocks
	// waiting for a handler that itself is waiting for r.Context().Done.
	p := newTestProvider(t, func(w http.ResponseWriter, r *http.Request) {
		t.Fatal("handler reached despite pre-cancelled ctx")
	})

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	_, err := p.ChatCompletion(ctx, basicRequest())
	require.Error(t, err)
	assert.True(t, errors.Is(err, context.Canceled))
	// ctx.Canceled is not a retryable category.
	assert.False(t, provider.IsRetryable(err))
}

func TestChatCompletion_ContextTimeout(t *testing.T) {
	// Handler sleeps longer than the caller ctx timeout; verifies that
	// ctx.DeadlineExceeded is mapped to ErrTimeout (retryable).
	p := newTestProvider(t, func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-time.After(5 * time.Second):
		case <-r.Context().Done():
		}
	})

	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()

	_, err := p.ChatCompletion(ctx, basicRequest())
	require.Error(t, err)
	assert.True(t, errors.Is(err, provider.ErrTimeout))
	assert.True(t, provider.IsRetryable(err))
}

func TestChatCompletion_MalformedSuccessBody(t *testing.T) {
	// 200 OK but body isn't valid JSON.
	p := newTestProvider(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, `not-json`)
	})

	_, err := p.ChatCompletion(context.Background(), basicRequest())
	require.Error(t, err)
	assert.True(t, errors.Is(err, provider.ErrUpstream))
}

func TestChatCompletion_NilRequest(t *testing.T) {
	p, _ := New(Config{Name: "openai", APIKey: "sk-x"})
	_, err := p.ChatCompletion(context.Background(), nil)
	require.Error(t, err)
	assert.True(t, errors.Is(err, provider.ErrInvalidRequest))
}
