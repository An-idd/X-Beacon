package anthropic

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"

	"github.com/An-idd/x-beacon/internal/provider"
)

// ChatCompletion issues a non-streaming Messages request. The caller's
// OpenAI-shaped ChatRequest is converted to Anthropic's wire format;
// system messages are extracted into the top-level "system" field, and
// max_tokens is defaulted to 4096 when the caller omits it.
func (p *Provider) ChatCompletion(ctx context.Context, req *provider.ChatRequest) (*provider.ChatResponse, error) {
	if req == nil {
		return nil, provider.NewUpstreamError(p.cfg.Name, provider.ErrInvalidRequest, 0, "nil request")
	}

	anthReq := toAnthropicRequest(req, p.defaultMaxTokens, false)

	body, err := json.Marshal(anthReq)
	if err != nil {
		return nil, fmt.Errorf("anthropic: marshal request: %w", err)
	}

	ctx, cancel := context.WithTimeout(ctx, p.cfg.Timeout)
	defer cancel()

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, p.baseURL+messagesPath, bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("anthropic: build request: %w", err)
	}
	p.setHeaders(httpReq)

	resp, err := p.httpClient.Do(httpReq)
	if err != nil {
		return nil, mapRequestError(p.cfg.Name, err)
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, mapRequestError(p.cfg.Name, err)
	}

	if resp.StatusCode >= 400 {
		return nil, mapHTTPError(p.cfg.Name, resp.StatusCode, respBody, resp.Header)
	}

	var parsed messagesResponse
	if err := json.Unmarshal(respBody, &parsed); err != nil {
		return nil, provider.NewUpstreamError(p.cfg.Name, provider.ErrUpstream, resp.StatusCode, "decode response: "+err.Error())
	}
	return fromAnthropicResponse(&parsed, p.cfg.Name), nil
}
