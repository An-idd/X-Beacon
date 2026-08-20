package anthropic

import (
	"bytes"
	"context"
	"net/http"
)

// ForwardMessages POSTs an Anthropic-native /v1/messages request body to
// the upstream verbatim — no parse, no re-marshal — so protocol features
// the gateway doesn't model (thinking blocks, prompt caching, beta
// features) survive byte-identical in both directions. betaHeader, when
// non-empty, is the client's anthropic-beta value passed through as-is.
//
// The caller owns resp.Body (streaming responses stay open until read).
// No timeout is applied here: streams outlive any sane fixed timeout;
// cancellation rides on ctx (client disconnect) like ChatCompletionStream.
func (p *Provider) ForwardMessages(ctx context.Context, body []byte, betaHeader string) (*http.Response, error) {
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, p.baseURL+messagesPath, bytes.NewReader(body))
	if err != nil {
		return nil, mapRequestError(p.cfg.Name, err)
	}
	p.setHeaders(httpReq)
	if betaHeader != "" {
		httpReq.Header.Set("anthropic-beta", betaHeader)
	}
	resp, err := p.httpClient.Do(httpReq)
	if err != nil {
		return nil, mapRequestError(p.cfg.Name, err)
	}
	return resp, nil
}
