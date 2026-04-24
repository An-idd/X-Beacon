// Package registry builds and owns the set of Provider adapters the
// gateway routes to. A Registry is constructed once at startup via Load
// from a YAML file; resolution is lock-free thereafter.
package registry

import (
	"errors"
	"fmt"
	"path"
	"sort"

	"github.com/An-idd/x-beacon/internal/provider"
)

var (
	// ErrUnknownProvider is returned by GetByName when the name is not
	// registered.
	ErrUnknownProvider = errors.New("registry: unknown provider")
	// ErrNoProviderForModel is returned by ResolveModel when no exact,
	// glob, or default match can be found.
	ErrNoProviderForModel = errors.New("registry: no provider matches model")
)

// Registry owns the set of provider adapters and routes model IDs to them.
// After Load returns, the Registry is safe for concurrent read use.
type Registry struct {
	// Stable ordering for Names(); matches providers.yaml declaration order.
	names []string

	// name → Provider
	byName map[string]provider.Provider

	// exact model ID → Provider. Populated during Load; duplicates across
	// providers cause Load to fail (startup error).
	exactIndex map[string]provider.Provider

	// Glob rules in providers.yaml declaration order. First match wins.
	globRules []globRule

	// Default provider, used when neither exact nor glob match. May be nil
	// when default_provider was unset.
	defaultProvider provider.Provider
}

type globRule struct {
	pattern  string
	provider provider.Provider
}

// NewEmpty returns a Registry with no providers. Useful when main starts
// without a providers.yaml file (bootstrapping / smoke tests). All lookup
// methods return a well-typed "no match" error; AllModels returns nil.
func NewEmpty() *Registry {
	return &Registry{
		byName:     make(map[string]provider.Provider),
		exactIndex: make(map[string]provider.Provider),
	}
}

// GetByName looks up a provider by its configured name (e.g. "openai-primary").
func (r *Registry) GetByName(name string) (provider.Provider, error) {
	p, ok := r.byName[name]
	if !ok {
		return nil, fmt.Errorf("%w: %q", ErrUnknownProvider, name)
	}
	return p, nil
}

// ResolveModel selects the provider responsible for the given model ID
// using the three-tier priority:
//   - exact match (highest)
//   - first glob match in providers.yaml declaration order
//   - default_provider (fallback)
//
// Returns ErrNoProviderForModel if no tier matches.
func (r *Registry) ResolveModel(model string) (provider.Provider, error) {
	if p, ok := r.exactIndex[model]; ok {
		return p, nil
	}
	for _, rule := range r.globRules {
		matched, err := path.Match(rule.pattern, model)
		if err != nil {
			// A malformed pattern should have been rejected at Load time;
			// we skip rather than error so one broken rule doesn't block
			// resolution of others.
			continue
		}
		if matched {
			return rule.provider, nil
		}
	}
	if r.defaultProvider != nil {
		return r.defaultProvider, nil
	}
	return nil, fmt.Errorf("%w: %q", ErrNoProviderForModel, model)
}

// Names returns all registered provider names in declaration order.
func (r *Registry) Names() []string {
	out := make([]string, len(r.names))
	copy(out, r.names)
	return out
}

// AllModels returns the union of every provider's SupportedModels, sorted
// by ID for stable /v1/models responses. Duplicates (same model ID from
// different providers) keep the first occurrence by declaration order.
func (r *Registry) AllModels() []provider.ModelInfo {
	seen := make(map[string]struct{})
	out := make([]provider.ModelInfo, 0)
	for _, name := range r.names {
		for _, m := range r.byName[name].SupportedModels() {
			if _, dup := seen[m.ID]; dup {
				continue
			}
			seen[m.ID] = struct{}{}
			out = append(out, m)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}
