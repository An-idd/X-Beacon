// Package deepseek adapts DeepSeek's OpenAI-compatible chat API to the
// gateway's Provider interface. The wire protocol matches OpenAI's, so
// this package is a thin wrapper around internal/provider/openai; it
// exists to (a) keep registry/metrics attribution distinct from direct
// OpenAI usage and (b) carry DeepSeek-specific defaults (endpoint etc.).
//
// If DeepSeek diverges from the OpenAI wire format in the future, that
// logic belongs here rather than mutating the openai adapter.
package deepseek

import (
	"time"

	"github.com/An-idd/x-beacon/internal/provider/openai"
)

const defaultEndpoint = "https://api.deepseek.com"

// Config mirrors openai.Config minus fields DeepSeek doesn't use
// (e.g. OpenAI-Organization header).
type Config struct {
	Name     string
	Endpoint string
	APIKey   string
	Timeout  time.Duration
	Models   openai.Models
}

// New constructs a DeepSeek-backed provider. The returned value is an
// *openai.Provider; Name() carries cfg.Name so metrics/logs can tell
// DeepSeek apart from a direct OpenAI instance.
func New(cfg Config) (*openai.Provider, error) {
	if cfg.Endpoint == "" {
		cfg.Endpoint = defaultEndpoint
	}
	return openai.New(openai.Config{
		Name:     cfg.Name,
		Endpoint: cfg.Endpoint,
		APIKey:   cfg.APIKey,
		Timeout:  cfg.Timeout,
		Models:   cfg.Models,
		OwnedBy:  "deepseek",
	})
}
