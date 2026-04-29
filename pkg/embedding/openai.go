package embedding

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/An-idd/x-beacon/internal/provider"
	"github.com/An-idd/x-beacon/pkg/version"
)

const (
	openAIDefaultEndpoint   = "https://api.openai.com"
	openAIDefaultModel      = "text-embedding-3-small"
	openAIDefaultDimensions = 1536
	openAIDefaultTimeout    = 30 * time.Second
	openAIEmbedPath         = "/v1/embeddings"
	maxEmbeddedBodyLen      = 200
)

// OpenAIConfig parameterizes a NewOpenAI client. Endpoint / Model /
// Dimensions / Timeout default sensibly when zero; APIKey is required.
type OpenAIConfig struct {
	// APIKey is the OpenAI bearer token. Required.
	APIKey string

	// Endpoint defaults to https://api.openai.com. Overridable for
	// proxies / Azure compatibility / test stubs.
	Endpoint string

	// Model selects the embedding model. Default
	// "text-embedding-3-small" (1536 dims, ~$0.02 per 1M tokens).
	Model string

	// Dimensions truncates the returned vector. Only "v3" embeddings
	// support this — pass 0 to receive the model's native dimension.
	// Lower values save Redis index memory at the cost of recall.
	Dimensions int

	// Timeout caps a single Embed() HTTP round-trip. Default 30 s. The
	// caller's ctx still wins if it expires sooner.
	Timeout time.Duration
}

// OpenAI is an Embedder backed by OpenAI's /v1/embeddings endpoint.
// Safe for concurrent use after construction.
type OpenAI struct {
	cfg        OpenAIConfig
	httpClient *http.Client
	dimensions int
}

// NewOpenAI validates cfg and constructs the client. Errors only
// surface misconfiguration; per-request failures happen at Embed().
func NewOpenAI(cfg OpenAIConfig) (*OpenAI, error) {
	if cfg.APIKey == "" {
		return nil, errors.New("embedding/openai: APIKey is required")
	}
	if cfg.Endpoint == "" {
		cfg.Endpoint = openAIDefaultEndpoint
	}
	if cfg.Model == "" {
		cfg.Model = openAIDefaultModel
	}
	if cfg.Timeout <= 0 {
		cfg.Timeout = openAIDefaultTimeout
	}
	dim := cfg.Dimensions
	if dim <= 0 {
		dim = openAIDefaultDimensions
	}
	return &OpenAI{
		cfg: cfg,
		// Per-request deadline lives in ctx (set inside Embed) — leaving
		// http.Client.Timeout at zero so streaming/embeddings share the
		// same client style as internal/provider/openai.
		httpClient: &http.Client{},
		dimensions: dim,
	}, nil
}

// Dimensions reports the vector length this client produces. Constant
// for the lifetime of the OpenAI value.
func (o *OpenAI) Dimensions() int { return o.dimensions }

// Model returns the configured model id (e.g. text-embedding-3-small).
func (o *OpenAI) Model() string { return o.cfg.Model }

// embedRequest mirrors POST /v1/embeddings body. Input may be a single
// string or array per OpenAI spec; we always send an array for shape
// stability.
type embedRequest struct {
	Input          []string `json:"input"`
	Model          string   `json:"model"`
	EncodingFormat string   `json:"encoding_format,omitempty"`
	Dimensions     int      `json:"dimensions,omitempty"`
}

// embedResponse covers the success body. data[].index lets us defend
// against unordered responses (the spec says ordered; we double-check).
type embedResponse struct {
	Data []struct {
		Index     int       `json:"index"`
		Embedding []float32 `json:"embedding"`
	} `json:"data"`
	Model string `json:"model"`
}

// Embed POSTs texts to /v1/embeddings and returns one float32 vector
// per input. Input is propagated verbatim — the caller is responsible
// for any normalization or truncation upstream of this call.
func (o *OpenAI) Embed(ctx context.Context, texts []string) ([][]float32, error) {
	if len(texts) == 0 {
		return nil, ErrEmptyInput
	}

	body := embedRequest{
		Input: texts,
		Model: o.cfg.Model,
	}
	if o.cfg.Dimensions > 0 {
		body.Dimensions = o.cfg.Dimensions
	}
	buf, err := json.Marshal(body)
	if err != nil {
		return nil, fmt.Errorf("embedding/openai: marshal: %w", err)
	}

	callCtx, cancel := context.WithTimeout(ctx, o.cfg.Timeout)
	defer cancel()

	url := strings.TrimRight(o.cfg.Endpoint, "/") + openAIEmbedPath
	req, err := http.NewRequestWithContext(callCtx, http.MethodPost, url, bytes.NewReader(buf))
	if err != nil {
		return nil, fmt.Errorf("embedding/openai: build request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+o.cfg.APIKey)
	req.Header.Set("User-Agent", "x-beacon/"+version.Version)

	resp, err := o.httpClient.Do(req)
	if err != nil {
		return nil, mapEmbedRequestError(err)
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, provider.NewUpstreamError("openai-embeddings", provider.ErrUpstream, resp.StatusCode,
			"read response body: "+err.Error())
	}

	if resp.StatusCode != http.StatusOK {
		return nil, mapEmbedHTTPError(resp.StatusCode, respBody)
	}

	var out embedResponse
	if err := json.Unmarshal(respBody, &out); err != nil {
		return nil, provider.NewUpstreamError("openai-embeddings", provider.ErrUpstream, resp.StatusCode,
			"decode embeddings response: "+err.Error())
	}
	if len(out.Data) != len(texts) {
		return nil, provider.NewUpstreamError("openai-embeddings", provider.ErrUpstream, resp.StatusCode,
			fmt.Sprintf("response length mismatch: sent %d, got %d", len(texts), len(out.Data)))
	}

	// Defensive sort by Index so we never hand back a vector for the
	// wrong input position. OpenAI returns sorted but spec doesn't
	// strictly mandate it.
	sort.SliceStable(out.Data, func(i, j int) bool { return out.Data[i].Index < out.Data[j].Index })

	vecs := make([][]float32, len(texts))
	for i, d := range out.Data {
		if len(d.Embedding) != o.dimensions {
			return nil, provider.NewUpstreamError("openai-embeddings", provider.ErrUpstream, resp.StatusCode,
				fmt.Sprintf("dimension mismatch at index %d: want %d, got %d", i, o.dimensions, len(d.Embedding)))
		}
		vecs[i] = d.Embedding
	}
	return vecs, nil
}

// embedAPIError matches OpenAI's standard error envelope. Same shape
// as internal/provider/openai's apiError but kept local so this
// package can sit in pkg/ without importing the chat provider.
type embedAPIError struct {
	Error struct {
		Message string `json:"message"`
		Type    string `json:"type"`
		Code    string `json:"code"`
	} `json:"error"`
}

func mapEmbedHTTPError(status int, body []byte) error {
	var ae embedAPIError
	_ = json.Unmarshal(body, &ae)

	sentinel := provider.ErrUpstream
	switch {
	case status == http.StatusUnauthorized, status == http.StatusForbidden:
		sentinel = provider.ErrAuth
	case status == http.StatusTooManyRequests:
		sentinel = provider.ErrRateLimited
	case status == http.StatusBadRequest, status == http.StatusUnprocessableEntity:
		sentinel = provider.ErrInvalidRequest
	case status >= 500 && status < 600:
		sentinel = provider.ErrUpstream
	}

	msg := ae.Error.Message
	if msg == "" {
		msg = string(body)
		if len(msg) > maxEmbeddedBodyLen {
			msg = msg[:maxEmbeddedBodyLen] + "..."
		}
	}
	return provider.NewUpstreamError("openai-embeddings", sentinel, status, msg)
}

func mapEmbedRequestError(err error) error {
	if errors.Is(err, context.Canceled) {
		return err
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return provider.NewUpstreamError("openai-embeddings", provider.ErrTimeout, 0, "request deadline exceeded")
	}
	return provider.NewUpstreamError("openai-embeddings", provider.ErrUnavailable, 0, err.Error())
}

// Compile-time interface guard.
var _ Embedder = (*OpenAI)(nil)
