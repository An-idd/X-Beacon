package anthropic

import (
	"strings"
	"time"

	"github.com/An-idd/x-beacon/internal/provider"
)

// --- Wire types -------------------------------------------------------------

// messagesRequest is Anthropic's /v1/messages request body.
type messagesRequest struct {
	Model         string     `json:"model"`
	Messages      []anthMsg  `json:"messages"`
	System        string     `json:"system,omitempty"`
	MaxTokens     int        `json:"max_tokens"` // required by Anthropic
	Temperature   *float64   `json:"temperature,omitempty"`
	TopP          *float64   `json:"top_p,omitempty"`
	StopSequences []string   `json:"stop_sequences,omitempty"`
	Stream        bool       `json:"stream,omitempty"`
}

// anthMsg is an Anthropic message. Only user/assistant roles are allowed;
// system prompts live in the top-level System field of messagesRequest.
type anthMsg struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

// messagesResponse is Anthropic's non-streaming response body.
type messagesResponse struct {
	ID           string         `json:"id"`
	Type         string         `json:"type"` // "message"
	Role         string         `json:"role"` // "assistant"
	Content      []contentBlock `json:"content"`
	Model        string         `json:"model"`
	StopReason   string         `json:"stop_reason"`
	StopSequence string         `json:"stop_sequence,omitempty"`
	Usage        messageUsage   `json:"usage"`
}

type contentBlock struct {
	Type string `json:"type"` // "text" in Week 2 scope
	Text string `json:"text"`
}

type messageUsage struct {
	InputTokens  int `json:"input_tokens"`
	OutputTokens int `json:"output_tokens"`
}

// --- Request conversion -----------------------------------------------------

// toAnthropicRequest translates a gateway ChatRequest into Anthropic's wire
// format. System messages are lifted to the top-level System field (joined
// with \n\n if multiple). The returned request always has Stream set per
// the stream arg — caller's req.Stream value is ignored.
func toAnthropicRequest(req *provider.ChatRequest, defaultMaxTokens int, stream bool) *messagesRequest {
	out := &messagesRequest{
		Model:         req.Model,
		Temperature:   req.Temperature,
		TopP:          req.TopP,
		StopSequences: req.Stop,
		Stream:        stream,
	}

	var systems []string
	for _, m := range req.Messages {
		if m.Role == "system" {
			if m.Content != "" {
				systems = append(systems, m.Content)
			}
			continue
		}
		out.Messages = append(out.Messages, anthMsg{
			Role:    m.Role,
			Content: m.Content,
		})
	}
	if len(systems) > 0 {
		out.System = strings.Join(systems, "\n\n")
	}

	if req.MaxTokens > 0 {
		out.MaxTokens = req.MaxTokens
	} else {
		out.MaxTokens = defaultMaxTokens
	}

	return out
}

// --- Response conversion ----------------------------------------------------

// fromAnthropicResponse translates Anthropic's non-streaming response back
// to the gateway's OpenAI-shaped ChatResponse. All text blocks are
// concatenated; non-text blocks are ignored (future-proofing for multimodal
// and tool_use without breaking MVP).
func fromAnthropicResponse(resp *messagesResponse, providerName string) *provider.ChatResponse {
	var content strings.Builder
	for _, b := range resp.Content {
		if b.Type == "text" {
			content.WriteString(b.Text)
		}
	}

	return &provider.ChatResponse{
		ID:      resp.ID,
		Object:  "chat.completion",
		Created: time.Now().Unix(), // Anthropic doesn't send this; synthesize
		Model:   resp.Model,
		Choices: []provider.Choice{{
			Index: 0,
			Message: provider.Message{
				Role:    "assistant",
				Content: content.String(),
			},
			FinishReason: mapStopReason(resp.StopReason),
		}},
		Usage: &provider.Usage{
			PromptTokens:     resp.Usage.InputTokens,
			CompletionTokens: resp.Usage.OutputTokens,
			TotalTokens:      resp.Usage.InputTokens + resp.Usage.OutputTokens,
		},
		Provider: providerName,
	}
}

// mapStopReason maps Anthropic's stop_reason to OpenAI's finish_reason.
// Unknown values pass through unchanged so callers can inspect them; this
// is lossy-safe because finish_reason is a free-form string in practice.
func mapStopReason(r string) string {
	switch r {
	case "end_turn":
		return "stop"
	case "max_tokens":
		return "length"
	case "stop_sequence":
		return "stop"
	case "tool_use":
		return "tool_calls"
	default:
		return r
	}
}
