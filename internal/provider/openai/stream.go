package openai

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/An-idd/x-beacon/internal/provider"
)

// Compile-time check that *Provider satisfies provider.Provider.
var _ provider.Provider = (*Provider)(nil)

// sseLineBufferLimit caps a single SSE line at 1 MiB. OpenAI chunks are
// typically <4 KB, but Anthropic-style tool_use events can be larger;
// pick a value comfortably above both and bounded against runaway allocs.
const sseLineBufferLimit = 1 << 20

// dataPrefix is the SSE field we care about. Per spec a single space after
// the colon is optional — trim it if present.
const dataPrefix = "data:"

// streamWirePayload decodes a single SSE chunk into either a normal
// ChatStreamChunk or a mid-stream error envelope in one pass. OpenAI
// occasionally sends `data: {"error": {...}}` after the response has
// started; without the Error field here, a straight ChatStreamChunk
// decode succeeds but yields an empty chunk (no ID, no Choices), which
// the consumer cannot distinguish from a legitimate delta.
type streamWirePayload struct {
	provider.ChatStreamChunk
	Error *openaiErrorPayload `json:"error,omitempty"`
}

// ChatCompletionStream issues a streaming chat request. The caller receives
// a channel of StreamEvent; the producer closes the channel on termination
// (success, upstream error, or ctx cancellation). The returned error is
// non-nil only when the HTTP request fails before the stream starts.
func (p *Provider) ChatCompletionStream(ctx context.Context, req *provider.ChatRequest) (<-chan provider.StreamEvent, error) {
	if req == nil {
		return nil, provider.NewUpstreamError(p.cfg.Name, provider.ErrInvalidRequest, 0, "nil request")
	}

	// Copy so we don't mutate caller's struct; force Stream=true.
	r := *req
	r.Stream = true

	body, err := json.Marshal(&r)
	if err != nil {
		return nil, fmt.Errorf("openai: marshal request: %w", err)
	}

	// Streams are long-lived; do NOT wrap ctx with cfg.Timeout. Upstream
	// controls termination via [DONE]; caller controls abort via ctx.
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, p.baseURL+chatPath, bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("openai: build request: %w", err)
	}
	p.setHeaders(httpReq)
	httpReq.Header.Set("Accept", "text/event-stream")

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

// readSSE parses the SSE response body line-by-line and forwards chunks to
// ch. It is responsible for closing both ch and body on every exit path.
//
// Exit paths and their channel outcomes:
//   - [DONE] received          → close(ch), no event
//   - ctx cancelled            → close(ch), no event (silent, honors intent)
//   - malformed chunk JSON     → emit StreamEvent{Err: ErrUpstream}, close(ch)
//   - scanner I/O error        → emit StreamEvent{Err: ErrUpstream}, close(ch)
//   - EOF without [DONE]       → emit StreamEvent{Err: ErrUpstream}, close(ch)
func (p *Provider) readSSE(ctx context.Context, body io.ReadCloser, ch chan<- provider.StreamEvent) {
	defer close(ch)
	defer body.Close()

	sc := bufio.NewScanner(body)
	sc.Buffer(make([]byte, 0, 4096), sseLineBufferLimit)

	for sc.Scan() {
		line := sc.Text()

		// Empty line = event boundary; ":" = comment (keepalive etc.).
		if line == "" || strings.HasPrefix(line, ":") {
			continue
		}
		if !strings.HasPrefix(line, dataPrefix) {
			continue
		}
		data := strings.TrimPrefix(line[len(dataPrefix):], " ")

		if data == "[DONE]" {
			return
		}

		var payload streamWirePayload
		if err := json.Unmarshal([]byte(data), &payload); err != nil {
			p.sendEvent(ctx, ch, provider.StreamEvent{
				Err: provider.NewUpstreamError(p.cfg.Name, provider.ErrUpstream, 0, "decode chunk: "+err.Error()),
			})
			return
		}

		// Mid-stream error envelope: surface as terminal error event.
		// Status is 0 because the HTTP response itself was 2xx — the error
		// is reported inline after streaming began.
		if payload.Error != nil {
			sentinel := mapStatusToSentinel(0, payload.Error.Code)
			p.sendEvent(ctx, ch, provider.StreamEvent{
				Err: provider.NewUpstreamError(p.cfg.Name, sentinel, 0, payload.Error.Message),
			})
			return
		}

		chunk := payload.ChatStreamChunk
		if !p.sendEvent(ctx, ch, provider.StreamEvent{Chunk: &chunk}) {
			return
		}
	}

	// Scanner exited. Three remaining possibilities:
	//   1) ctx cancelled — silent close
	//   2) scanner I/O error — surface as ErrUpstream
	//   3) clean EOF without [DONE] — surface as ErrUpstream (stream truncated)
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
		Err: provider.NewUpstreamError(p.cfg.Name, provider.ErrUpstream, 0, "stream ended without [DONE]"),
	})
}

// sendEvent attempts to deliver ev but respects ctx cancellation. Returns
// false iff ctx was cancelled before the send completed; callers use this
// to abort the read loop without leaking goroutines on slow consumers.
func (p *Provider) sendEvent(ctx context.Context, ch chan<- provider.StreamEvent, ev provider.StreamEvent) bool {
	select {
	case ch <- ev:
		return true
	case <-ctx.Done():
		return false
	}
}
