package anthropic

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

// anthropicSuccessBody is a real-shape /v1/messages response.
const anthropicSuccessBody = `{
	"id": "msg_01ABCDEFGH",
	"type": "message",
	"role": "assistant",
	"model": "claude-3-5-sonnet-20241022",
	"content": [{"type":"text","text":"Hello from Claude"}],
	"stop_reason": "end_turn",
	"usage": {"input_tokens": 12, "output_tokens": 4}
}`

func basicRequest() *provider.ChatRequest {
	return &provider.ChatRequest{
		Model:    "claude-3-5-sonnet-20241022",
		Messages: []provider.Message{{Role: "user", Content: "hi"}},
	}
}

func TestChatCompletion_Success(t *testing.T) {
	p := newTestProvider(t, func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, http.MethodPost, r.Method)
		assert.Equal(t, messagesPath, r.URL.Path)
		assert.Equal(t, "sk-ant-test", r.Header.Get("x-api-key"))
		assert.Equal(t, defaultAPIVersion, r.Header.Get("anthropic-version"))
		assert.Empty(t, r.Header.Get("Authorization"), "must not use Authorization header")

		var inbound messagesRequest
		body, _ := io.ReadAll(r.Body)
		require.NoError(t, json.Unmarshal(body, &inbound))
		assert.False(t, inbound.Stream)                // non-streaming path
		assert.Equal(t, defaultMaxTokens, inbound.MaxTokens) // caller omitted → default

		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, anthropicSuccessBody)
	})

	resp, err := p.ChatCompletion(context.Background(), basicRequest())
	require.NoError(t, err)
	require.NotNil(t, resp)
	assert.Equal(t, "msg_01ABCDEFGH", resp.ID)
	assert.Equal(t, "anthropic-test", resp.Provider)
	require.Len(t, resp.Choices, 1)
	assert.Equal(t, "Hello from Claude", resp.Choices[0].Message.Content)
	assert.Equal(t, "stop", resp.Choices[0].FinishReason) // mapped from "end_turn"
	require.NotNil(t, resp.Usage)
	assert.Equal(t, 12, resp.Usage.PromptTokens)
	assert.Equal(t, 4, resp.Usage.CompletionTokens)
	assert.Equal(t, 16, resp.Usage.TotalTokens)
}

func TestChatCompletion_SystemMessageExtracted(t *testing.T) {
	p := newTestProvider(t, func(w http.ResponseWriter, r *http.Request) {
		var inbound messagesRequest
		body, _ := io.ReadAll(r.Body)
		require.NoError(t, json.Unmarshal(body, &inbound))

		// Verify system extracted to top-level, not in messages array.
		assert.Equal(t, "You are helpful.", inbound.System)
		require.Len(t, inbound.Messages, 1)
		assert.Equal(t, "user", inbound.Messages[0].Role)
		assert.Equal(t, "hi", inbound.Messages[0].Content)

		_, _ = io.WriteString(w, anthropicSuccessBody)
	})

	req := &provider.ChatRequest{
		Model: "claude-3-5-sonnet-20241022",
		Messages: []provider.Message{
			{Role: "system", Content: "You are helpful."},
			{Role: "user", Content: "hi"},
		},
	}
	_, err := p.ChatCompletion(context.Background(), req)
	require.NoError(t, err)
}

func TestChatCompletion_MultipleSystemMessages_Joined(t *testing.T) {
	p := newTestProvider(t, func(w http.ResponseWriter, r *http.Request) {
		var inbound messagesRequest
		body, _ := io.ReadAll(r.Body)
		require.NoError(t, json.Unmarshal(body, &inbound))
		assert.Equal(t, "You are helpful.\n\nBe concise.", inbound.System)
		_, _ = io.WriteString(w, anthropicSuccessBody)
	})

	req := &provider.ChatRequest{
		Messages: []provider.Message{
			{Role: "system", Content: "You are helpful."},
			{Role: "user", Content: "hi"},
			{Role: "system", Content: "Be concise."},
		},
	}
	_, err := p.ChatCompletion(context.Background(), req)
	require.NoError(t, err)
}

func TestChatCompletion_CallerMaxTokensPreserved(t *testing.T) {
	p := newTestProvider(t, func(w http.ResponseWriter, r *http.Request) {
		var inbound messagesRequest
		body, _ := io.ReadAll(r.Body)
		require.NoError(t, json.Unmarshal(body, &inbound))
		assert.Equal(t, 256, inbound.MaxTokens)
		_, _ = io.WriteString(w, anthropicSuccessBody)
	})

	req := basicRequest()
	req.MaxTokens = 256
	_, err := p.ChatCompletion(context.Background(), req)
	require.NoError(t, err)
}

func TestChatCompletion_Error_401(t *testing.T) {
	p := newTestProvider(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = io.WriteString(w, `{"type":"error","error":{"type":"authentication_error","message":"invalid x-api-key"}}`)
	})

	_, err := p.ChatCompletion(context.Background(), basicRequest())
	require.Error(t, err)
	assert.True(t, errors.Is(err, provider.ErrAuth))
	assert.False(t, provider.IsRetryable(err))
}

func TestChatCompletion_Error_429_RateLimit(t *testing.T) {
	p := newTestProvider(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Retry-After", "10")
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = io.WriteString(w, `{"type":"error","error":{"type":"rate_limit_error","message":"slow down"}}`)
	})

	_, err := p.ChatCompletion(context.Background(), basicRequest())
	require.Error(t, err)
	assert.True(t, errors.Is(err, provider.ErrRateLimited))
	var ue *provider.UpstreamError
	require.True(t, errors.As(err, &ue))
	assert.Equal(t, 10*time.Second, ue.RetryAfter)
}

func TestChatCompletion_Error_529_Overloaded(t *testing.T) {
	// Anthropic's "Overloaded" with HTTP 529 and error.type=overloaded_error.
	p := newTestProvider(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Retry-After", "5")
		w.WriteHeader(529)
		_, _ = io.WriteString(w, `{"type":"error","error":{"type":"overloaded_error","message":"overloaded"}}`)
	})

	_, err := p.ChatCompletion(context.Background(), basicRequest())
	require.Error(t, err)
	assert.True(t, errors.Is(err, provider.ErrUnavailable))
	assert.True(t, provider.IsRetryable(err))

	var ue *provider.UpstreamError
	require.True(t, errors.As(err, &ue))
	assert.Equal(t, 5*time.Second, ue.RetryAfter)
}

func TestChatCompletion_Error_RequestTooLarge(t *testing.T) {
	p := newTestProvider(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusRequestEntityTooLarge)
		_, _ = io.WriteString(w, `{"type":"error","error":{"type":"request_too_large","message":"too many tokens"}}`)
	})

	_, err := p.ChatCompletion(context.Background(), basicRequest())
	require.Error(t, err)
	assert.True(t, errors.Is(err, provider.ErrContextLength))
	assert.False(t, provider.IsRetryable(err))
}

func TestChatCompletion_Error_500(t *testing.T) {
	p := newTestProvider(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = io.WriteString(w, `{"type":"error","error":{"type":"api_error","message":"server exploded"}}`)
	})

	_, err := p.ChatCompletion(context.Background(), basicRequest())
	require.Error(t, err)
	assert.True(t, errors.Is(err, provider.ErrUpstream))
	assert.True(t, provider.IsRetryable(err))
}

func TestChatCompletion_ContextCanceled(t *testing.T) {
	p := newTestProvider(t, func(w http.ResponseWriter, r *http.Request) {
		t.Fatal("handler reached despite pre-cancelled ctx")
	})

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	_, err := p.ChatCompletion(ctx, basicRequest())
	require.Error(t, err)
	assert.True(t, errors.Is(err, context.Canceled))
}

func TestChatCompletion_ContextTimeout(t *testing.T) {
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
	p := newTestProvider(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, `not-json`)
	})

	_, err := p.ChatCompletion(context.Background(), basicRequest())
	require.Error(t, err)
	assert.True(t, errors.Is(err, provider.ErrUpstream))
}

func TestChatCompletion_NilRequest(t *testing.T) {
	p, _ := New(Config{Name: "x", APIKey: "sk-x"})
	_, err := p.ChatCompletion(context.Background(), nil)
	require.Error(t, err)
	assert.True(t, errors.Is(err, provider.ErrInvalidRequest))
}
