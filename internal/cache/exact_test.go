package cache

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/An-idd/x-beacon/internal/provider"
)

func ptrFloat(v float64) *float64 { return &v }

func TestKey_DeterministicForLogicallyEqualRequest(t *testing.T) {
	a := &provider.ChatRequest{
		Model:       "gpt-4o",
		Messages:    []provider.Message{{Role: "user", Content: "ping"}},
		Temperature: ptrFloat(0.7),
		MaxTokens:   100,
	}
	b := &provider.ChatRequest{
		Model:       "gpt-4o",
		Messages:    []provider.Message{{Role: "user", Content: "ping"}},
		Temperature: ptrFloat(0.7),
		MaxTokens:   100,
	}
	ka, err := Key(a)
	require.NoError(t, err)
	kb, err := Key(b)
	require.NoError(t, err)
	assert.Equal(t, ka, kb)
	assert.Contains(t, ka, "cache:exact:")
	assert.Len(t, ka, len("cache:exact:")+64) // sha256 hex
}

func TestKey_StreamFlagDoesNotAffectKey(t *testing.T) {
	// Decision 1: stream=true/false share the same key (Week 10 will
	// replay cached non-stream responses as a synthetic stream).
	base := &provider.ChatRequest{
		Model:    "gpt-4o",
		Messages: []provider.Message{{Role: "user", Content: "ping"}},
	}
	withStream := *base
	withStream.Stream = true

	k1, err := Key(base)
	require.NoError(t, err)
	k2, err := Key(&withStream)
	require.NoError(t, err)
	assert.Equal(t, k1, k2)
}

func TestKey_DiffersOnSensitiveInputs(t *testing.T) {
	base := &provider.ChatRequest{
		Model:       "gpt-4o",
		Messages:    []provider.Message{{Role: "user", Content: "ping"}},
		Temperature: ptrFloat(0.7),
		MaxTokens:   100,
	}
	baseKey, err := Key(base)
	require.NoError(t, err)

	cases := []struct {
		name   string
		mutate func(*provider.ChatRequest)
	}{
		{"different model", func(r *provider.ChatRequest) { r.Model = "gpt-4o-mini" }},
		{"different message content", func(r *provider.ChatRequest) {
			r.Messages = []provider.Message{{Role: "user", Content: "pong"}}
		}},
		{"different role", func(r *provider.ChatRequest) {
			r.Messages = []provider.Message{{Role: "system", Content: "ping"}}
		}},
		{"extra message", func(r *provider.ChatRequest) {
			r.Messages = append(r.Messages, provider.Message{Role: "assistant", Content: "ok"})
		}},
		{"different temperature", func(r *provider.ChatRequest) { r.Temperature = ptrFloat(0.0) }},
		{"different top_p", func(r *provider.ChatRequest) { r.TopP = ptrFloat(0.5) }},
		{"different max_tokens", func(r *provider.ChatRequest) { r.MaxTokens = 200 }},
		{"different stop", func(r *provider.ChatRequest) { r.Stop = []string{"END"} }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cp := *base
			// Re-copy slices we mutate so cases don't leak into base.
			cp.Messages = append([]provider.Message(nil), base.Messages...)
			tc.mutate(&cp)
			k, err := Key(&cp)
			require.NoError(t, err)
			assert.NotEqual(t, baseKey, k)
		})
	}
}

func TestKey_NilTemperatureSameAsAbsent(t *testing.T) {
	// omitempty means a nil pointer hashes the same as "field omitted".
	a := &provider.ChatRequest{Model: "x", Messages: []provider.Message{{Role: "user", Content: "y"}}}
	b := *a
	b.Temperature = nil
	ka, err := Key(a)
	require.NoError(t, err)
	kb, err := Key(&b)
	require.NoError(t, err)
	assert.Equal(t, ka, kb)
}

func TestKey_NilRequestErrors(t *testing.T) {
	_, err := Key(nil)
	assert.Error(t, err)
}

func newTestExact(t *testing.T) (*RedisExact, *miniredis.Miniredis) {
	t.Helper()
	mr := miniredis.RunT(t)
	client := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = client.Close() })
	return NewRedisExact(client), mr
}

func TestRedisExact_GetSetRoundTrip(t *testing.T) {
	exact, _ := newTestExact(t)
	resp := &provider.ChatResponse{
		ID:      "chatcmpl-1",
		Object:  "chat.completion",
		Model:   "gpt-4o",
		Choices: []provider.Choice{{Index: 0, Message: provider.Message{Role: "assistant", Content: "ok"}, FinishReason: "stop"}},
		Usage:   &provider.Usage{PromptTokens: 1, CompletionTokens: 1, TotalTokens: 2},
	}
	require.NoError(t, exact.Set(context.Background(), "k", resp, time.Minute))

	got, err := exact.Get(context.Background(), "k")
	require.NoError(t, err)
	assert.Equal(t, resp.ID, got.ID)
	assert.Equal(t, resp.Choices[0].Message.Content, got.Choices[0].Message.Content)
	assert.Equal(t, resp.Usage.TotalTokens, got.Usage.TotalTokens)
}

func TestRedisExact_GetMiss(t *testing.T) {
	exact, _ := newTestExact(t)
	_, err := exact.Get(context.Background(), "missing")
	assert.True(t, errors.Is(err, ErrMiss))
}

func TestRedisExact_TTLApplied(t *testing.T) {
	exact, mr := newTestExact(t)
	resp := &provider.ChatResponse{ID: "x"}
	require.NoError(t, exact.Set(context.Background(), "k", resp, time.Minute))
	mr.FastForward(2 * time.Minute)
	_, err := exact.Get(context.Background(), "k")
	assert.True(t, errors.Is(err, ErrMiss))
}

func TestRedisExact_SetRejectsNonPositiveTTL(t *testing.T) {
	exact, _ := newTestExact(t)
	err := exact.Set(context.Background(), "k", &provider.ChatResponse{}, 0)
	assert.Error(t, err)
}

func TestRedisExact_SetRejectsNilResponse(t *testing.T) {
	exact, _ := newTestExact(t)
	err := exact.Set(context.Background(), "k", nil, time.Minute)
	assert.Error(t, err)
}

func TestRedisExact_Delete(t *testing.T) {
	exact, _ := newTestExact(t)
	require.NoError(t, exact.Set(context.Background(), "k", &provider.ChatResponse{ID: "x"}, time.Minute))
	require.NoError(t, exact.Delete(context.Background(), "k"))
	_, err := exact.Get(context.Background(), "k")
	assert.True(t, errors.Is(err, ErrMiss))
	// Deleting a missing key is not an error.
	require.NoError(t, exact.Delete(context.Background(), "k"))
}

func TestRedisExact_GetCorruptEntryReturnsErrorNotMiss(t *testing.T) {
	exact, mr := newTestExact(t)
	require.NoError(t, mr.Set("k", "{not-json"))
	_, err := exact.Get(context.Background(), "k")
	require.Error(t, err)
	assert.False(t, errors.Is(err, ErrMiss), "decode error should be distinguishable from miss")
}

func TestNewRedisExact_NilClientPanics(t *testing.T) {
	assert.Panics(t, func() { NewRedisExact(nil) })
}
