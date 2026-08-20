// Package registry builds and owns the set of Provider adapters the
// gateway routes to. A Registry is constructed once at startup via Load
// from a YAML file; resolution is lock-free thereafter.
package registry

import (
	"errors"
	"fmt"
	"path"
	"sort"
	"sync/atomic"

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
// Reads are lock-free; the entire provider table lives behind one atomic
// pointer so Swap can replace it wholesale at runtime (hot reload) without
// readers ever observing a half-updated table.
type Registry struct {
	state atomic.Pointer[registryState]
}

// registryState is the immutable provider table. Built once by Load /
// NewEmpty, never mutated afterwards — hot reload builds a fresh state
// and swaps the pointer.
type registryState struct {
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

// newRegistry wraps a built state in a Registry.
func newRegistry(st *registryState) *Registry {
	r := &Registry{}
	r.state.Store(st)
	return r
}

// Swap atomically replaces this Registry's provider table with from's.
// In-flight requests keep the table they already resolved against; new
// requests see the new one. Used by the /admin/reload endpoint.
func (r *Registry) Swap(from *Registry) {
	r.state.Store(from.state.Load())
}

type globRule struct {
	pattern  string
	provider provider.Provider
}

// NewEmpty returns a Registry with no providers. Useful when main starts
// without a providers.yaml file (bootstrapping / smoke tests). All lookup
// methods return a well-typed "no match" error; AllModels returns nil.
func NewEmpty() *Registry {
	return newRegistry(&registryState{
		byName:     make(map[string]provider.Provider),
		exactIndex: make(map[string]provider.Provider),
	})
}

// GetByName looks up a provider by its configured name (e.g. "openai-primary").
func (r *Registry) GetByName(name string) (provider.Provider, error) {
	p, ok := r.state.Load().byName[name]
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
	chain := r.ResolveChain(model)
	if len(chain) == 0 {
		return nil, fmt.Errorf("%w: %q", ErrNoProviderForModel, model)
	}
	return chain[0], nil
}

// ResolveChain returns every provider that COULD serve the given model, in
// priority order: exact owner first, then each matching glob in
// declaration order, then default_provider. Duplicates are removed (a
// provider that both exact-matches and glob-matches appears once at its
// highest priority position).
//
// Used by the router (Week 6) to fail over to the next-best provider when
// the primary returns a retryable error. Returns nil when no tier matches.
func (r *Registry) ResolveChain(model string) []provider.Provider {
	st := r.state.Load()
	chain := make([]provider.Provider, 0, 3)
	seen := make(map[string]struct{}, 3)
	add := func(p provider.Provider) {
		if p == nil {
			return
		}
		if _, dup := seen[p.Name()]; dup {
			return
		}
		seen[p.Name()] = struct{}{}
		chain = append(chain, p)
	}

	if p, ok := st.exactIndex[model]; ok {
		add(p)
	}
	for _, rule := range st.globRules {
		matched, err := path.Match(rule.pattern, model)
		if err != nil {
			// A malformed pattern should have been rejected at Load time;
			// skip rather than error so one broken rule doesn't block
			// resolution of others.
			continue
		}
		if matched {
			add(rule.provider)
		}
	}
	add(st.defaultProvider)
	return chain
}

// Names returns all registered provider names in declaration order.
func (r *Registry) Names() []string {
	st := r.state.Load()
	out := make([]string, len(st.names))
	copy(out, st.names)
	return out
}

// AllModels returns the union of every provider's SupportedModels, sorted
// by ID for stable /v1/models responses. Duplicates (same model ID from
// different providers) keep the first occurrence by declaration order.
func (r *Registry) AllModels() []provider.ModelInfo {
	st := r.state.Load()
	seen := make(map[string]struct{})
	out := make([]provider.ModelInfo, 0)
	for _, name := range st.names {
		for _, m := range st.byName[name].SupportedModels() {
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
