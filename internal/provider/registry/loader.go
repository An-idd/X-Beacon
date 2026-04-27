package registry

import (
	"errors"
	"fmt"
	"os"
	"path"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/An-idd/x-beacon/internal/envexpand"
	"github.com/An-idd/x-beacon/internal/provider"
	"github.com/An-idd/x-beacon/internal/provider/anthropic"
	"github.com/An-idd/x-beacon/internal/provider/deepseek"
	"github.com/An-idd/x-beacon/internal/provider/openai"
)

// providersFile is the top-level schema of providers.yaml.
type providersFile struct {
	Providers       []providerConfig `yaml:"providers"`
	DefaultProvider string           `yaml:"default_provider"`
}

// providerConfig is the per-provider YAML entry. Env-var placeholders in
// string fields are expanded during Load.
type providerConfig struct {
	Name         string        `yaml:"name"`
	Type         string        `yaml:"type"`
	Endpoint     string        `yaml:"endpoint"`
	APIKey       string        `yaml:"api_key"`
	Organization string        `yaml:"organization"`
	Timeout      time.Duration `yaml:"timeout"`
	Priority     int           `yaml:"priority"` // stored for Week 6; unused in 1.3
	Models       modelsConfig  `yaml:"models"`
}

type modelsConfig struct {
	Exact []string `yaml:"exact"`
	Glob  []string `yaml:"glob"`
}

// Load reads providers.yaml from path, expands env-var placeholders, and
// constructs the Registry. All validation errors are collected per-provider
// where possible to surface multiple problems at once.
func Load(path string) (*Registry, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("registry: read %q: %w", path, err)
	}

	// Expand env before YAML parsing so ${VAR} inside quoted string values
	// becomes the expanded text. Structural YAML tokens are not disturbed
	// because our placeholder syntax (${...}) is not a YAML meta-character.
	raw = []byte(envexpand.Expand(string(raw)))

	var pf providersFile
	if err := yaml.Unmarshal(raw, &pf); err != nil {
		return nil, fmt.Errorf("registry: parse yaml: %w", err)
	}

	return build(&pf)
}

func build(pf *providersFile) (*Registry, error) {
	if len(pf.Providers) == 0 {
		return nil, errors.New("registry: providers list is empty")
	}

	reg := &Registry{
		names:      make([]string, 0, len(pf.Providers)),
		byName:     make(map[string]provider.Provider, len(pf.Providers)),
		exactIndex: make(map[string]provider.Provider),
		globRules:  make([]globRule, 0),
	}

	// exactOwner tracks which provider first claimed each exact model, so
	// the second claim produces a clear conflict message.
	exactOwner := make(map[string]string)

	var errs []error
	for i, pc := range pf.Providers {
		if err := validateProviderConfig(i, &pc); err != nil {
			errs = append(errs, err)
			continue
		}
		if _, dup := reg.byName[pc.Name]; dup {
			errs = append(errs, fmt.Errorf("providers[%d]: duplicate name %q", i, pc.Name))
			continue
		}
		p, err := constructProvider(&pc)
		if err != nil {
			errs = append(errs, fmt.Errorf("providers[%d] %q: %w", i, pc.Name, err))
			continue
		}
		reg.names = append(reg.names, pc.Name)
		reg.byName[pc.Name] = p

		for _, m := range pc.Models.Exact {
			if owner, taken := exactOwner[m]; taken {
				errs = append(errs, fmt.Errorf(
					"providers[%d] %q: model %q already claimed by %q (exact matches must be unique across providers)",
					i, pc.Name, m, owner))
				continue
			}
			exactOwner[m] = pc.Name
			reg.exactIndex[m] = p
		}
		for _, g := range pc.Models.Glob {
			if _, err := path.Match(g, ""); err != nil {
				errs = append(errs, fmt.Errorf("providers[%d] %q: invalid glob %q: %w", i, pc.Name, g, err))
				continue
			}
			reg.globRules = append(reg.globRules, globRule{pattern: g, provider: p})
		}
	}

	if pf.DefaultProvider != "" {
		p, ok := reg.byName[pf.DefaultProvider]
		if !ok {
			errs = append(errs, fmt.Errorf("default_provider %q not registered", pf.DefaultProvider))
		} else {
			reg.defaultProvider = p
		}
	}

	if len(errs) > 0 {
		return nil, fmt.Errorf("registry: load failed: %w", errors.Join(errs...))
	}
	return reg, nil
}

func validateProviderConfig(idx int, pc *providerConfig) error {
	var errs []error
	if pc.Name == "" {
		errs = append(errs, fmt.Errorf("providers[%d]: name is required", idx))
	}
	if pc.Type == "" {
		errs = append(errs, fmt.Errorf("providers[%d] %q: type is required", idx, pc.Name))
	}
	if len(pc.Models.Exact) == 0 && len(pc.Models.Glob) == 0 {
		errs = append(errs, fmt.Errorf("providers[%d] %q: at least one model (exact or glob) is required", idx, pc.Name))
	}
	return errors.Join(errs...)
}

// constructProvider dispatches on pc.Type to build the concrete adapter.
// New provider types register a branch here. Each adapter's own New()
// performs type-specific validation (e.g. api_key required for openai).
func constructProvider(pc *providerConfig) (provider.Provider, error) {
	switch pc.Type {
	case "openai":
		return openai.New(openai.Config{
			Name:         pc.Name,
			Endpoint:     pc.Endpoint,
			APIKey:       pc.APIKey,
			Organization: pc.Organization,
			Timeout:      pc.Timeout,
			Models: openai.Models{
				Exact: pc.Models.Exact,
				Glob:  pc.Models.Glob,
			},
		})
	case "deepseek":
		return deepseek.New(deepseek.Config{
			Name:     pc.Name,
			Endpoint: pc.Endpoint,
			APIKey:   pc.APIKey,
			Timeout:  pc.Timeout,
			Models: openai.Models{
				Exact: pc.Models.Exact,
				Glob:  pc.Models.Glob,
			},
		})
	case "anthropic":
		return anthropic.New(anthropic.Config{
			Name:     pc.Name,
			Endpoint: pc.Endpoint,
			APIKey:   pc.APIKey,
			Timeout:  pc.Timeout,
			Models: anthropic.Models{
				Exact: pc.Models.Exact,
				Glob:  pc.Models.Glob,
			},
		})
	default:
		return nil, fmt.Errorf("unknown provider type %q (supported: openai, deepseek, anthropic)", pc.Type)
	}
}
