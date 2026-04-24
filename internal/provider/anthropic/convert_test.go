package anthropic

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/An-idd/x-beacon/internal/provider"
)

func ptrFloat(v float64) *float64 { return &v }

func TestToAnthropicRequest_SystemExtracted(t *testing.T) {
	req := &provider.ChatRequest{
		Model: "claude-3-5-sonnet",
		Messages: []provider.Message{
			{Role: "system", Content: "You are helpful."},
			{Role: "user", Content: "Hi"},
		},
	}
	out := toAnthropicRequest(req, 4096, false)

	assert.Equal(t, "You are helpful.", out.System)
	require.Len(t, out.Messages, 1)
	assert.Equal(t, "user", out.Messages[0].Role)
	assert.Equal(t, "Hi", out.Messages[0].Content)
}

func TestToAnthropicRequest_MultipleSystemsConcatenated(t *testing.T) {
	req := &provider.ChatRequest{
		Model: "claude-3-5-sonnet",
		Messages: []provider.Message{
			{Role: "system", Content: "You are helpful."},
			{Role: "user", Content: "Hi"},
			{Role: "system", Content: "Be concise."},
		},
	}
	out := toAnthropicRequest(req, 4096, false)

	assert.Equal(t, "You are helpful.\n\nBe concise.", out.System)
	// Only non-system messages remain in Messages.
	require.Len(t, out.Messages, 1)
	assert.Equal(t, "user", out.Messages[0].Role)
}

func TestToAnthropicRequest_EmptySystemsSkipped(t *testing.T) {
	req := &provider.ChatRequest{
		Messages: []provider.Message{
			{Role: "system", Content: ""},
			{Role: "user", Content: "Hi"},
		},
	}
	out := toAnthropicRequest(req, 4096, false)
	assert.Empty(t, out.System)
	require.Len(t, out.Messages, 1)
}

func TestToAnthropicRequest_DefaultMaxTokensApplied(t *testing.T) {
	req := &provider.ChatRequest{
		Messages: []provider.Message{{Role: "user", Content: "Hi"}},
	}
	out := toAnthropicRequest(req, 4096, false)
	assert.Equal(t, 4096, out.MaxTokens)
}

func TestToAnthropicRequest_CallerMaxTokensPreserved(t *testing.T) {
	req := &provider.ChatRequest{
		Messages:  []provider.Message{{Role: "user", Content: "Hi"}},
		MaxTokens: 500,
	}
	out := toAnthropicRequest(req, 4096, false)
	assert.Equal(t, 500, out.MaxTokens)
}

func TestToAnthropicRequest_StopToStopSequences(t *testing.T) {
	req := &provider.ChatRequest{
		Messages: []provider.Message{{Role: "user", Content: "Hi"}},
		Stop:     []string{"\n\n", "END"},
	}
	out := toAnthropicRequest(req, 4096, false)
	assert.Equal(t, []string{"\n\n", "END"}, out.StopSequences)
}

func TestToAnthropicRequest_StreamFlagForced(t *testing.T) {
	req := &provider.ChatRequest{
		Messages: []provider.Message{{Role: "user", Content: "Hi"}},
		Stream:   false, // caller says false
	}
	out := toAnthropicRequest(req, 4096, true) // but we force true
	assert.True(t, out.Stream)
}

func TestToAnthropicRequest_TemperatureZeroPreserved(t *testing.T) {
	req := &provider.ChatRequest{
		Messages:    []provider.Message{{Role: "user", Content: "Hi"}},
		Temperature: ptrFloat(0),
	}
	out := toAnthropicRequest(req, 4096, false)
	require.NotNil(t, out.Temperature)
	assert.Equal(t, 0.0, *out.Temperature)
}

func TestFromAnthropicResponse_SingleTextBlock(t *testing.T) {
	resp := &messagesResponse{
		ID:         "msg_1",
		Type:       "message",
		Role:       "assistant",
		Model:      "claude-3-5-sonnet-20241022",
		Content:    []contentBlock{{Type: "text", Text: "Hello!"}},
		StopReason: "end_turn",
		Usage:      messageUsage{InputTokens: 5, OutputTokens: 3},
	}
	out := fromAnthropicResponse(resp, "anthropic-primary")

	assert.Equal(t, "msg_1", out.ID)
	assert.Equal(t, "chat.completion", out.Object)
	assert.Equal(t, "anthropic-primary", out.Provider)
	require.Len(t, out.Choices, 1)
	assert.Equal(t, "assistant", out.Choices[0].Message.Role)
	assert.Equal(t, "Hello!", out.Choices[0].Message.Content)
	assert.Equal(t, "stop", out.Choices[0].FinishReason)
	require.NotNil(t, out.Usage)
	assert.Equal(t, 5, out.Usage.PromptTokens)
	assert.Equal(t, 3, out.Usage.CompletionTokens)
	assert.Equal(t, 8, out.Usage.TotalTokens)
}

func TestFromAnthropicResponse_MultipleTextBlocksConcatenated(t *testing.T) {
	resp := &messagesResponse{
		Content: []contentBlock{
			{Type: "text", Text: "Hello "},
			{Type: "text", Text: "world!"},
		},
		Usage: messageUsage{},
	}
	out := fromAnthropicResponse(resp, "x")
	assert.Equal(t, "Hello world!", out.Choices[0].Message.Content)
}

func TestFromAnthropicResponse_NonTextBlocksSkipped(t *testing.T) {
	// Forward-compat: image or tool_use blocks should not crash the parser.
	resp := &messagesResponse{
		Content: []contentBlock{
			{Type: "text", Text: "Before"},
			{Type: "tool_use", Text: "ignored_in_mvp"},
			{Type: "text", Text: "After"},
		},
		Usage: messageUsage{},
	}
	out := fromAnthropicResponse(resp, "x")
	assert.Equal(t, "BeforeAfter", out.Choices[0].Message.Content)
}

func TestFromAnthropicResponse_CreatedIsNonZero(t *testing.T) {
	resp := &messagesResponse{Content: []contentBlock{}, Usage: messageUsage{}}
	out := fromAnthropicResponse(resp, "x")
	assert.Positive(t, out.Created)
}

func TestMapStopReason(t *testing.T) {
	tests := map[string]string{
		"end_turn":      "stop",
		"max_tokens":    "length",
		"stop_sequence": "stop",
		"tool_use":      "tool_calls",
		"":              "",
		"unknown_value": "unknown_value", // passthrough
	}
	for in, want := range tests {
		t.Run(in, func(t *testing.T) {
			assert.Equal(t, want, mapStopReason(in))
		})
	}
}
