// Package openai adapts the OpenAI REST API to the gateway's generic
// provider.Provider interface. It targets the /v1/chat/completions endpoint;
// embeddings and other families are deferred to later phases.
package openai

import (
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/An-idd/x-beacon/internal/provider"
	"github.com/An-idd/x-beacon/pkg/version"
)

const (
	defaultEndpoint = "https://api.openai.com"
	defaultTimeout  = 60 * time.Second
	chatPath        = "/v1/chat/completions"
)

// Config describes one OpenAI-compatible provider instance. Multiple
// instances of this adapter may co-exist (e.g. openai-primary + azure-openai)
// distinguished by Name.
type Config struct {
	Name         string        // registry name, e.g. "openai-primary"
	Endpoint     string        // base URL; default "https://api.openai.com"
	APIKey       string        // required
	Organization string        // optional OpenAI-Organization header
	Timeout      time.Duration // per-request timeout for non-streaming; default 60s
	Models       Models        // driven by providers.yaml in Step 1.3
	OwnedBy      string        // /v1/models owned_by attribution; default "openai"
}

// Models enumerates what this provider advertises. Only Exact drives
// SupportedModels(); Glob is stored for the registry's routing logic.
type Models struct {
	Exact []string
	Glob  []string
}

// Provider is the OpenAI adapter. Safe for concurrent use after construction.
type Provider struct {
	cfg        Config
	baseURL    string
	httpClient *http.Client
}

// New validates Config and constructs a Provider. Errors surface only
// misconfiguration; network/runtime failures happen per-request.
func New(cfg Config) (*Provider, error) {
	if cfg.Name == "" {
		return nil, errors.New("openai: Config.Name is required")
	}
	if cfg.APIKey == "" {
		return nil, errors.New("openai: Config.APIKey is required")
	}
	if cfg.Endpoint == "" {
		cfg.Endpoint = defaultEndpoint
	}
	if cfg.Timeout == 0 {
		cfg.Timeout = defaultTimeout
	}
	if cfg.OwnedBy == "" {
		cfg.OwnedBy = "openai"
	}

	transport := &http.Transport{
		MaxIdleConns:          100,
		MaxIdleConnsPerHost:   10,
		IdleConnTimeout:       90 * time.Second,
		ExpectContinueTimeout: 1 * time.Second,
	}

	return &Provider{
		cfg:     cfg,
		baseURL: strings.TrimRight(cfg.Endpoint, "/"),
		// Timeout=0: we control deadlines via ctx so the same client works
		// for both streaming (indefinite body) and non-streaming paths.
		httpClient: &http.Client{Transport: transport},
	}, nil
}

func (p *Provider) Name() string { return p.cfg.Name }

// SupportedModels returns the Exact list as ModelInfo. Glob-matched models
// are intentionally excluded: /v1/models should only report models whose
// availability we can confirm up front.
func (p *Provider) SupportedModels() []provider.ModelInfo {
	out := make([]provider.ModelInfo, 0, len(p.cfg.Models.Exact))
	for _, id := range p.cfg.Models.Exact {
		out = append(out, provider.ModelInfo{
			ID:       id,
			Object:   "model",
			OwnedBy:  p.cfg.OwnedBy,
			Provider: p.cfg.Name,
		})
	}
	return out
}

// setHeaders applies auth, content negotiation, and identification headers.
func (p *Provider) setHeaders(req *http.Request) {
	req.Header.Set("Authorization", "Bearer "+p.cfg.APIKey)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", version.UserAgent())
	if p.cfg.Organization != "" {
		req.Header.Set("OpenAI-Organization", p.cfg.Organization)
	}
}
