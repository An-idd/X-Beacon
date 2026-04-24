package openai

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"

	"github.com/An-idd/x-beacon/internal/provider"
)

// ChatCompletion issues a non-streaming chat request to OpenAI. The
// outbound JSON body mirrors provider.ChatRequest (OpenAI-shaped); the
// caller's req.Stream value is ignored and forced to false.
func (p *Provider) ChatCompletion(ctx context.Context, req *provider.ChatRequest) (*provider.ChatResponse, error) {
	if req == nil {
		return nil, provider.NewUpstreamError(p.cfg.Name, provider.ErrInvalidRequest, 0, "nil request")
	}

	// Copy so we don't mutate caller's struct.
	r := *req
	r.Stream = false

	body, err := json.Marshal(&r)
	if err != nil {
		return nil, fmt.Errorf("openai: marshal request: %w", err)
	}

	ctx, cancel := context.WithTimeout(ctx, p.cfg.Timeout)
	defer cancel()

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, p.baseURL+chatPath, bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("openai: build request: %w", err)
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

	var out provider.ChatResponse
	if err := json.Unmarshal(respBody, &out); err != nil {
		return nil, provider.NewUpstreamError(p.cfg.Name, provider.ErrUpstream, resp.StatusCode, "decode response: "+err.Error())
	}
	out.Provider = p.cfg.Name
	return &out, nil
}
