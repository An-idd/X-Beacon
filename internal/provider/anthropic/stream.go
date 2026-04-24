package anthropic

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/An-idd/x-beacon/internal/provider"
)

// Compile-time check that *Provider satisfies provider.Provider.
var _ provider.Provider = (*Provider)(nil)

const (
	sseLineBufferLimit = 1 << 20 // 1 MiB per SSE line
	dataPrefix         = "data:"
)

// streamEvent is a flat capture of every Anthropic SSE event payload.
// Anthropic's events carry disjoint fields depending on Type, so one
// tagged struct covers all cases after a single json.Unmarshal. The
// event: header line is deliberately ignored — we dispatch on the JSON
// `type` field, which carries the authoritative value.
type streamEvent struct {
	Type string `json:"type"`

	// Populated only for message_start.
	Message *struct {
		ID    string       `json:"id"`
		Model string       `json:"model"`
		Usage messageUsage `json:"usage"`
	} `json:"message,omitempty"`

	// Populated for content_block_delta / content_block_start / content_block_stop.
	Index int `json:"index,omitempty"`

	// Delta has different shape per event type; fields don't collide:
	//   content_block_delta → Type="text_delta" + Text  (or input_json_delta, ignored)
	//   message_delta       → StopReason
	Delta *struct {
		Type       string `json:"type"`
		Text       string `json:"text,omitempty"`
		StopReason string `json:"stop_reason,omitempty"`
	} `json:"delta,omitempty"`

	// Populated only for message_delta.
	Usage *messageUsage `json:"usage,omitempty"`

	// Populated only for error events.
	Error *struct {
		Type    string `json:"type"`
		Message string `json:"message"`
	} `json:"error,omitempty"`
}

// anthStreamState carries per-stream metadata captured at message_start so
// that subsequent chunks can include consistent id/model/created fields.
// OpenAI's ChatStreamChunk has these per chunk; Anthropic sends them once.
type anthStreamState struct {
	providerName string
	id           string
	model        string
	created      int64
	inputTokens  int
}

func newStreamState(providerName string) *anthStreamState {
	return &anthStreamState{
		providerName: providerName,
		created:      time.Now().Unix(),
	}
}

// makeChunk starts a ChatStreamChunk pre-populated with the cached metadata.
// Caller fills in Choices and (optionally) Usage before sending.
func (s *anthStreamState) makeChunk() provider.ChatStreamChunk {
	return provider.ChatStreamChunk{
		ID:      s.id,
		Object:  "chat.completion.chunk",
		Created: s.created,
		Model:   s.model,
	}
}

// ChatCompletionStream issues a streaming Messages request. On success the
// returned channel emits a sequence of ChatStreamChunks that mimic OpenAI's
// streaming convention:
//
//   - Chunk 1: role-only delta (from message_start)
//   - Chunks 2..N-1: content deltas (from content_block_delta.text_delta)
//   - Chunk N: empty delta + finish_reason + usage (from message_delta)
//
// Non-text content blocks (e.g. input_json_delta for tool_use) are skipped.
func (p *Provider) ChatCompletionStream(ctx context.Context, req *provider.ChatRequest) (<-chan provider.StreamEvent, error) {
	if req == nil {
		return nil, provider.NewUpstreamError(p.cfg.Name, provider.ErrInvalidRequest, 0, "nil request")
	}

	anthReq := toAnthropicRequest(req, p.defaultMaxTokens, true)

	body, err := json.Marshal(anthReq)
	if err != nil {
		return nil, fmt.Errorf("anthropic: marshal request: %w", err)
	}

	// Streams are long-lived; do NOT wrap ctx with cfg.Timeout. Upstream
	// controls termination via message_stop; caller controls abort via ctx.
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, p.baseURL+messagesPath, bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("anthropic: build request: %w", err)
	}
	p.setHeaders(httpReq)
	httpReq.Header.Set("accept", "text/event-stream")

	resp, err := p.httpClient.Do(httpReq)
	if err != nil {
		return nil, mapRequestError(p.cfg.Name, err)
	}

	if resp.StatusCode >= 400 {
		respBody, _ := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		return nil, mapHTTPError(p.cfg.Name, resp.StatusCode, respBody, resp.Header)
	}

	ch := make(chan provider.StreamEvent, 16)
	go p.readSSE(ctx, resp.Body, ch)
	return ch, nil
}

// readSSE parses the SSE body line by line and dispatches each event via
// handleEvent. Five exit paths:
//
//   - message_stop received     → close(ch), no event
//   - ctx cancelled             → close(ch), no event
//   - error event received      → emit StreamEvent{Err}, close(ch)
//   - JSON decode failure       → emit StreamEvent{Err: ErrUpstream}, close(ch)
//   - EOF without message_stop  → emit StreamEvent{Err: ErrUpstream}, close(ch)
func (p *Provider) readSSE(ctx context.Context, body io.ReadCloser, ch chan<- provider.StreamEvent) {
	defer close(ch)
	defer body.Close()

	state := newStreamState(p.cfg.Name)
	sc := bufio.NewScanner(body)
	sc.Buffer(make([]byte, 0, 4096), sseLineBufferLimit)

	for sc.Scan() {
		line := sc.Text()

		// Blank line = event boundary; ":" = comment/keepalive; "event:" is
		// ignored because the JSON itself carries an authoritative type.
		if line == "" || strings.HasPrefix(line, ":") {
			continue
		}
		if !strings.HasPrefix(line, dataPrefix) {
			continue
		}
		data := strings.TrimPrefix(line[len(dataPrefix):], " ")

		var ev streamEvent
		if err := json.Unmarshal([]byte(data), &ev); err != nil {
			p.sendEvent(ctx, ch, provider.StreamEvent{
				Err: provider.NewUpstreamError(p.cfg.Name, provider.ErrUpstream, 0, "decode event: "+err.Error()),
			})
			return
		}

		done, ok := p.handleEvent(ctx, ch, state, &ev)
		if !ok {
			return // ctx cancelled during send
		}
		if done {
			return
		}
	}

	if ctx.Err() != nil {
		return
	}
	if err := sc.Err(); err != nil {
		p.sendEvent(ctx, ch, provider.StreamEvent{
			Err: provider.NewUpstreamError(p.cfg.Name, provider.ErrUpstream, 0, "stream read: "+err.Error()),
		})
		return
	}
	p.sendEvent(ctx, ch, provider.StreamEvent{
		Err: provider.NewUpstreamError(p.cfg.Name, provider.ErrUpstream, 0, "stream ended without message_stop"),
	})
}

// handleEvent dispatches on ev.Type. Returns (done, ok):
//   - done=true means readSSE should terminate (message_stop or terminal error)
//   - ok=false means ctx was cancelled while sending; readSSE must return immediately
//
// All emitted chunks use choices[0].index=0 regardless of Anthropic's
// content_block index, because OpenAI semantics reserve `index` for
// parallel choices (n>1), not for structural positioning within one reply.
func (p *Provider) handleEvent(ctx context.Context, ch chan<- provider.StreamEvent, state *anthStreamState, ev *streamEvent) (done, ok bool) {
	switch ev.Type {
	case "message_start":
		if ev.Message != nil {
			state.id = ev.Message.ID
			state.model = ev.Message.Model
			state.inputTokens = ev.Message.Usage.InputTokens
		}
		chunk := state.makeChunk()
		chunk.Choices = []provider.StreamChoice{{
			Index: 0,
			Delta: provider.MessageDelta{Role: "assistant"},
		}}
		return false, p.sendEvent(ctx, ch, provider.StreamEvent{Chunk: &chunk})

	case "content_block_delta":
		// Only text_delta carries user-visible content. input_json_delta
		// (tool_use streaming) is silently dropped in MVP.
		if ev.Delta == nil || ev.Delta.Type != "text_delta" || ev.Delta.Text == "" {
			return false, true
		}
		chunk := state.makeChunk()
		chunk.Choices = []provider.StreamChoice{{
			Index: 0,
			Delta: provider.MessageDelta{Content: ev.Delta.Text},
		}}
		return false, p.sendEvent(ctx, ch, provider.StreamEvent{Chunk: &chunk})

	case "message_delta":
		chunk := state.makeChunk()
		finishReason := ""
		if ev.Delta != nil {
			finishReason = mapStopReason(ev.Delta.StopReason)
		}
		chunk.Choices = []provider.StreamChoice{{
			Index:        0,
			FinishReason: finishReason,
		}}
		if ev.Usage != nil {
			chunk.Usage = &provider.Usage{
				PromptTokens:     state.inputTokens,
				CompletionTokens: ev.Usage.OutputTokens,
				TotalTokens:      state.inputTokens + ev.Usage.OutputTokens,
			}
		}
		return false, p.sendEvent(ctx, ch, provider.StreamEvent{Chunk: &chunk})

	case "message_stop":
		return true, true

	case "error":
		if ev.Error == nil {
			return true, p.sendEvent(ctx, ch, provider.StreamEvent{
				Err: provider.NewUpstreamError(p.cfg.Name, provider.ErrUpstream, 0, "upstream sent 'error' event with no body"),
			})
		}
		sentinel := mapToSentinel(0, ev.Error.Type)
		return true, p.sendEvent(ctx, ch, provider.StreamEvent{
			Err: provider.NewUpstreamError(p.cfg.Name, sentinel, 0, ev.Error.Message),
		})

	case "ping", "content_block_start", "content_block_stop":
		return false, true

	default:
		// Forward-compat: unknown event types don't break the stream.
		return false, true
	}
}

// sendEvent attempts to deliver ev but respects ctx cancellation. Returns
// false iff ctx was cancelled during the send.
func (p *Provider) sendEvent(ctx context.Context, ch chan<- provider.StreamEvent, ev provider.StreamEvent) bool {
	select {
	case ch <- ev:
		return true
	case <-ctx.Done():
		return false
	}
}
