// Package anthropic adapts the Anthropic Messages API to the gateway's
// Provider interface. Unlike DeepSeek, Anthropic's wire protocol is NOT
// OpenAI-compatible — request/response/stream formats all differ — so
// this package maintains its own wire types and conversion layer.
//
// Week 2 Step 2.2 implements non-streaming. Streaming (Step 2.3) adds
// the multi-event SSE state machine in stream.go.
package anthropic

import (
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/An-idd/x-beacon/internal/provider"
)

const (
	defaultEndpoint   = "https://api.anthropic.com"
	defaultTimeout    = 60 * time.Second
	defaultAPIVersion = "2023-06-01"
	defaultMaxTokens  = 4096
	messagesPath      = "/v1/messages"
	userAgent         = "x-beacon"
)

// Config describes one Anthropic provider instance.
type Config struct {
	Name             string        // registry name, e.g. "anthropic-primary"
	Endpoint         string        // base URL; default "https://api.anthropic.com"
	APIKey           string        // required
	APIVersion       string        // anthropic-version header; default "2023-06-01"
	Timeout          time.Duration // non-streaming request timeout; default 60s
	DefaultMaxTokens int           // applied when caller omits max_tokens; default 4096
	Models           Models
}

// Models mirrors the openai.Models layout for schema parity across adapters.
type Models struct {
	Exact []string
	Glob  []string
}

// Provider is the Anthropic adapter. Safe for concurrent use after construction.
type Provider struct {
	cfg              Config
	baseURL          string
	httpClient       *http.Client
	defaultMaxTokens int
}

// New validates Config and constructs a Provider.
func New(cfg Config) (*Provider, error) {
	if cfg.Name == "" {
		return nil, errors.New("anthropic: Config.Name is required")
	}
	if cfg.APIKey == "" {
		return nil, errors.New("anthropic: Config.APIKey is required")
	}
	if cfg.Endpoint == "" {
		cfg.Endpoint = defaultEndpoint
	}
	if cfg.APIVersion == "" {
		cfg.APIVersion = defaultAPIVersion
	}
	if cfg.Timeout == 0 {
		cfg.Timeout = defaultTimeout
	}
	if cfg.DefaultMaxTokens == 0 {
		cfg.DefaultMaxTokens = defaultMaxTokens
	}

	transport := &http.Transport{
		MaxIdleConns:          100,
		MaxIdleConnsPerHost:   10,
		IdleConnTimeout:       90 * time.Second,
		ExpectContinueTimeout: 1 * time.Second,
	}

	return &Provider{
		cfg:              cfg,
		baseURL:          strings.TrimRight(cfg.Endpoint, "/"),
		httpClient:       &http.Client{Transport: transport},
		defaultMaxTokens: cfg.DefaultMaxTokens,
	}, nil
}

func (p *Provider) Name() string { return p.cfg.Name }

// SupportedModels returns the Exact list, owned_by="anthropic".
func (p *Provider) SupportedModels() []provider.ModelInfo {
	out := make([]provider.ModelInfo, 0, len(p.cfg.Models.Exact))
	for _, id := range p.cfg.Models.Exact {
		out = append(out, provider.ModelInfo{
			ID:       id,
			Object:   "model",
			OwnedBy:  "anthropic",
			Provider: p.cfg.Name,
		})
	}
	return out
}

// setHeaders applies Anthropic's auth + required version + content negotiation.
// Note: Anthropic uses "x-api-key" (not "Authorization: Bearer") and requires
// the "anthropic-version" header on every request.
func (p *Provider) setHeaders(req *http.Request) {
	req.Header.Set("x-api-key", p.cfg.APIKey)
	req.Header.Set("anthropic-version", p.cfg.APIVersion)
	req.Header.Set("content-type", "application/json")
	req.Header.Set("accept", "application/json")
	req.Header.Set("user-agent", userAgent)
}

