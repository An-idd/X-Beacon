package provider

import (
	"encoding/json"
	"fmt"
)

// ChatRequest is the minimum-viable request shape shared across providers.
// Unknown fields from the client are preserved in Extra and forwarded
// opaquely to the upstream; struct fields always win over Extra on marshal.
type ChatRequest struct {
	Model       string                     `json:"model"`
	Messages    []Message                  `json:"messages"`
	MaxTokens   int                        `json:"max_tokens,omitempty"`
	Temperature *float64                   `json:"temperature,omitempty"` // pointer: 0 is a valid, meaningful value
	TopP        *float64                   `json:"top_p,omitempty"`
	N           int                        `json:"n,omitempty"`
	Stream      bool                       `json:"stream,omitempty"`
	Stop        []string                   `json:"stop,omitempty"`
	User        string                     `json:"user,omitempty"`
	Extra       map[string]json.RawMessage `json:"-"`
}

// Known keys on ChatRequest. Kept as a package var so UnmarshalJSON can
// filter them out of Extra in O(1) lookups.
var chatRequestKnownKeys = map[string]struct{}{
	"model": {}, "messages": {}, "max_tokens": {},
	"temperature": {}, "top_p": {}, "n": {},
	"stream": {}, "stop": {}, "user": {},
}

// chatRequestAlias prevents MarshalJSON/UnmarshalJSON recursion.
type chatRequestAlias ChatRequest

// UnmarshalJSON populates known fields from the standard decoder, then
// collects any unknown keys into Extra.
func (r *ChatRequest) UnmarshalJSON(data []byte) error {
	var a chatRequestAlias
	if err := json.Unmarshal(data, &a); err != nil {
		return err
	}
	*r = ChatRequest(a)

	var raw map[string]json.RawMessage
	if err := json.Unmarshal(data, &raw); err != nil {
		return err
	}
	for k := range chatRequestKnownKeys {
		delete(raw, k)
	}
	if len(raw) > 0 {
		r.Extra = raw
	}
	return nil
}

// MarshalJSON serializes the known fields and merges Extra into the output.
// Struct fields always take precedence over matching Extra keys (defensive;
// UnmarshalJSON already strips known keys from Extra).
func (r ChatRequest) MarshalJSON() ([]byte, error) {
	base, err := json.Marshal(chatRequestAlias(r))
	if err != nil {
		return nil, err
	}
	if len(r.Extra) == 0 {
		return base, nil
	}
	var merged map[string]json.RawMessage
	if err := json.Unmarshal(base, &merged); err != nil {
		return nil, err
	}
	for k, v := range r.Extra {
		if _, reserved := chatRequestKnownKeys[k]; reserved {
			continue
		}
		merged[k] = v
	}
	return json.Marshal(merged)
}

// Message is a single turn in a chat. Content is string-only in Week 1;
// multimodal (array of content parts) is deferred to later phases.
//
// Extra preserves unknown fields verbatim so OpenAI features the gateway
// doesn't model semantically (notably tool_calls, refusal, audio) survive
// the round-trip from upstream → gateway → client without being silently
// dropped. Same pattern as ChatRequest.Extra; struct fields always win
// over Extra on marshal.
type Message struct {
	Role       string                     `json:"role"`
	Content    string                     `json:"content"`
	Name       string                     `json:"name,omitempty"`
	ToolCallID string                     `json:"tool_call_id,omitempty"`
	Extra      map[string]json.RawMessage `json:"-"`
}

var messageKnownKeys = map[string]struct{}{
	"role": {}, "content": {}, "name": {}, "tool_call_id": {},
}

type messageAlias Message

func (m *Message) UnmarshalJSON(data []byte) error {
	var a messageAlias
	if err := json.Unmarshal(data, &a); err != nil {
		return err
	}
	*m = Message(a)
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(data, &raw); err != nil {
		return err
	}
	for k := range messageKnownKeys {
		delete(raw, k)
	}
	if len(raw) > 0 {
		m.Extra = raw
	}
	return nil
}

func (m Message) MarshalJSON() ([]byte, error) {
	// Encode known fields; omitempty handles empty Content for tool_call
	// responses (where role=assistant, content="", tool_calls populated).
	base, err := json.Marshal(messageAlias(m))
	if err != nil {
		return nil, err
	}
	if len(m.Extra) == 0 {
		return base, nil
	}
	var merged map[string]json.RawMessage
	if err := json.Unmarshal(base, &merged); err != nil {
		return nil, err
	}
	for k, v := range m.Extra {
		if _, reserved := messageKnownKeys[k]; reserved {
			continue
		}
		merged[k] = v
	}
	return json.Marshal(merged)
}

// ChatResponse is the non-streaming response returned by a Provider.
type ChatResponse struct {
	ID       string   `json:"id"`
	Object   string   `json:"object"`
	Created  int64    `json:"created"`
	Model    string   `json:"model"`
	Choices  []Choice `json:"choices"`
	Usage    *Usage   `json:"usage,omitempty"`
	Provider string   `json:"-"` // gateway-added; not exposed upstream/downstream
}

type Choice struct {
	Index        int                        `json:"index"`
	Message      Message                    `json:"message"`
	FinishReason string                     `json:"finish_reason,omitempty"`
	Extra        map[string]json.RawMessage `json:"-"`
}

var choiceKnownKeys = map[string]struct{}{
	"index": {}, "message": {}, "finish_reason": {},
}

type choiceAlias Choice

func (c *Choice) UnmarshalJSON(data []byte) error {
	var a choiceAlias
	if err := json.Unmarshal(data, &a); err != nil {
		return err
	}
	*c = Choice(a)
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(data, &raw); err != nil {
		return err
	}
	for k := range choiceKnownKeys {
		delete(raw, k)
	}
	if len(raw) > 0 {
		c.Extra = raw
	}
	return nil
}

func (c Choice) MarshalJSON() ([]byte, error) {
	base, err := json.Marshal(choiceAlias(c))
	if err != nil {
		return nil, err
	}
	if len(c.Extra) == 0 {
		return base, nil
	}
	var merged map[string]json.RawMessage
	if err := json.Unmarshal(base, &merged); err != nil {
		return nil, err
	}
	for k, v := range c.Extra {
		if _, reserved := choiceKnownKeys[k]; reserved {
			continue
		}
		merged[k] = v
	}
	return json.Marshal(merged)
}

// Usage reports token accounting as returned by the provider. May be nil
// if the provider doesn't include it; the billing pipeline is responsible
// for computing a local estimate in that case.
type Usage struct {
	PromptTokens     int `json:"prompt_tokens"`
	CompletionTokens int `json:"completion_tokens"`
	TotalTokens      int `json:"total_tokens"`
}

// ChatStreamChunk is one frame of a streaming response. A terminal chunk
// may carry finish_reason or (provider-dependent) usage.
type ChatStreamChunk struct {
	ID      string         `json:"id"`
	Object  string         `json:"object"`
	Created int64          `json:"created"`
	Model   string         `json:"model"`
	Choices []StreamChoice `json:"choices"`
	Usage   *Usage         `json:"usage,omitempty"`
}

type StreamChoice struct {
	Index        int          `json:"index"`
	Delta        MessageDelta `json:"delta"`
	FinishReason string       `json:"finish_reason,omitempty"`
}

// MessageDelta carries the incremental message fields during streaming.
// Extra preserves unknown delta fields (tool_calls deltas, refusal deltas)
// verbatim — same passthrough trick as Message.Extra and for the same
// reason: streaming tool-call args arrive as deltas and would be lost if
// the struct didn't capture them. Modeling tool semantics is deferred to
// Phase 6; for now we keep the wire faithful.
type MessageDelta struct {
	Role    string                     `json:"role,omitempty"`
	Content string                     `json:"content,omitempty"`
	Extra   map[string]json.RawMessage `json:"-"`
}

var messageDeltaKnownKeys = map[string]struct{}{
	"role": {}, "content": {},
}

type messageDeltaAlias MessageDelta

func (d *MessageDelta) UnmarshalJSON(data []byte) error {
	var a messageDeltaAlias
	if err := json.Unmarshal(data, &a); err != nil {
		return err
	}
	*d = MessageDelta(a)
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(data, &raw); err != nil {
		return err
	}
	for k := range messageDeltaKnownKeys {
		delete(raw, k)
	}
	if len(raw) > 0 {
		d.Extra = raw
	}
	return nil
}

func (d MessageDelta) MarshalJSON() ([]byte, error) {
	base, err := json.Marshal(messageDeltaAlias(d))
	if err != nil {
		return nil, err
	}
	if len(d.Extra) == 0 {
		return base, nil
	}
	var merged map[string]json.RawMessage
	if err := json.Unmarshal(base, &merged); err != nil {
		return nil, err
	}
	for k, v := range d.Extra {
		if _, reserved := messageDeltaKnownKeys[k]; reserved {
			continue
		}
		merged[k] = v
	}
	return json.Marshal(merged)
}

// StreamEvent is exactly one of {Chunk, Err}. Consumers range over the
// channel returned by ChatCompletionStream; the first event with non-nil
// Err is terminal (producer closes the channel after sending it).
type StreamEvent struct {
	Chunk *ChatStreamChunk
	Err   error
}

// String provides a safe debug representation without dumping prompts.
func (r *ChatRequest) String() string {
	return fmt.Sprintf("ChatRequest{model=%s, messages=%d, stream=%v}", r.Model, len(r.Messages), r.Stream)
}
