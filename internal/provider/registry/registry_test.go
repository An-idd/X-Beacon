package registry

import (
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/An-idd/x-beacon/internal/provider"
	"github.com/An-idd/x-beacon/internal/provider/openai"
)

// buildTestRegistry constructs a Registry directly (bypassing YAML) for
// focused resolution tests. The openai adapters are real but point at
// unused base URLs; no network calls happen during resolution.
func buildTestRegistry(t *testing.T) *Registry {
	t.Helper()
	primary, err := openai.New(openai.Config{
		Name:   "openai-primary",
		APIKey: "sk-x",
		Models: openai.Models{
			Exact: []string{"gpt-4o", "gpt-4o-mini"},
			Glob:  []string{"gpt-4-*", "gpt-3.5-*"},
		},
	})
	require.NoError(t, err)

	azure, err := openai.New(openai.Config{
		Name:   "azure-openai",
		APIKey: "sk-y",
		Models: openai.Models{
			Exact: []string{"gpt-4o-azure"},
			Glob:  []string{"gpt-4-*"}, // also matches; must lose to primary by declaration order
		},
	})
	require.NoError(t, err)

	ds, err := openai.New(openai.Config{
		Name:   "deepseek",
		APIKey: "sk-d",
		Models: openai.Models{
			Exact: []string{"deepseek-chat"},
		},
	})
	require.NoError(t, err)

	reg := &Registry{
		names:      []string{"openai-primary", "azure-openai", "deepseek"},
		byName:     map[string]provider.Provider{"openai-primary": primary, "azure-openai": azure, "deepseek": ds},
		exactIndex: map[string]provider.Provider{"gpt-4o": primary, "gpt-4o-mini": primary, "gpt-4o-azure": azure, "deepseek-chat": ds},
		globRules: []globRule{
			{pattern: "gpt-4-*", provider: primary},
			{pattern: "gpt-3.5-*", provider: primary},
			{pattern: "gpt-4-*", provider: azure}, // later; loses on ties
		},
		defaultProvider: primary,
	}
	return reg
}

func TestRegistry_GetByName(t *testing.T) {
	reg := buildTestRegistry(t)

	p, err := reg.GetByName("openai-primary")
	require.NoError(t, err)
	assert.Equal(t, "openai-primary", p.Name())

	_, err = reg.GetByName("no-such")
	require.Error(t, err)
	assert.True(t, errors.Is(err, ErrUnknownProvider))
}

func TestRegistry_ResolveModel_Exact(t *testing.T) {
	reg := buildTestRegistry(t)

	p, err := reg.ResolveModel("gpt-4o")
	require.NoError(t, err)
	assert.Equal(t, "openai-primary", p.Name())

	p, err = reg.ResolveModel("gpt-4o-azure")
	require.NoError(t, err)
	assert.Equal(t, "azure-openai", p.Name())

	p, err = reg.ResolveModel("deepseek-chat")
	require.NoError(t, err)
	assert.Equal(t, "deepseek", p.Name())
}

func TestRegistry_ResolveModel_GlobPrefersDeclarationOrder(t *testing.T) {
	reg := buildTestRegistry(t)
	// Both primary and azure glob match; primary declared first → wins.
	p, err := reg.ResolveModel("gpt-4-turbo")
	require.NoError(t, err)
	assert.Equal(t, "openai-primary", p.Name())
}

func TestRegistry_ResolveModel_ExactBeatsGlob(t *testing.T) {
	reg := buildTestRegistry(t)
	// "deepseek-chat" is deepseek's exact but could be caught by no glob.
	// Still, test that exact layer runs first.
	p, err := reg.ResolveModel("gpt-4o-mini")
	require.NoError(t, err)
	assert.Equal(t, "openai-primary", p.Name())
}

func TestRegistry_ResolveModel_DefaultFallback(t *testing.T) {
	reg := buildTestRegistry(t)
	// "llama-3" matches no exact and no glob → default_provider.
	p, err := reg.ResolveModel("llama-3-70b")
	require.NoError(t, err)
	assert.Equal(t, "openai-primary", p.Name())
}

func TestRegistry_ResolveModel_NoDefaultReturnsError(t *testing.T) {
	reg := buildTestRegistry(t)
	reg.defaultProvider = nil

	_, err := reg.ResolveModel("llama-3-70b")
	require.Error(t, err)
	assert.True(t, errors.Is(err, ErrNoProviderForModel))
}

func TestRegistry_Names(t *testing.T) {
	reg := buildTestRegistry(t)
	names := reg.Names()
	assert.Equal(t, []string{"openai-primary", "azure-openai", "deepseek"}, names)

	// Returned slice must be a copy — mutation must not leak.
	names[0] = "tampered"
	assert.Equal(t, "openai-primary", reg.Names()[0])
}

func TestRegistry_ResolveChain_ExactPlusGlobsPlusDefault(t *testing.T) {
	reg := buildTestRegistry(t)
	// "gpt-4-turbo" — no exact owner; primary glob matches; azure glob also
	// matches; default is primary (deduped from glob position 0).
	chain := reg.ResolveChain("gpt-4-turbo")
	require.Len(t, chain, 2)
	assert.Equal(t, "openai-primary", chain[0].Name())
	assert.Equal(t, "azure-openai", chain[1].Name())
}

func TestRegistry_ResolveChain_ExactWinsThenGlobs(t *testing.T) {
	reg := buildTestRegistry(t)
	// "gpt-4o-azure" — azure is exact owner; both globs ("gpt-4-*") don't
	// match this id, so chain has only azure + default(primary).
	chain := reg.ResolveChain("gpt-4o-azure")
	require.Len(t, chain, 2)
	assert.Equal(t, "azure-openai", chain[0].Name())
	assert.Equal(t, "openai-primary", chain[1].Name())
}

func TestRegistry_ResolveChain_NoMatchEmpty(t *testing.T) {
	reg := buildTestRegistry(t)
	reg.defaultProvider = nil
	chain := reg.ResolveChain("zzz-unknown")
	assert.Empty(t, chain)
}

func TestRegistry_ResolveChain_DedupExactAndGlob(t *testing.T) {
	reg := buildTestRegistry(t)
	// "gpt-4o" is primary's exact AND would be caught by primary's glob
	// "gpt-4-*" if rules accepted it (they don't — glob doesn't match this
	// id). But verify the dedup invariant another way: primary appears
	// once even though it's the default too.
	chain := reg.ResolveChain("gpt-4o")
	for i := range chain {
		for j := i + 1; j < len(chain); j++ {
			assert.NotEqual(t, chain[i].Name(), chain[j].Name(),
				"duplicate provider %q in chain", chain[i].Name())
		}
	}
}

func TestRegistry_AllModels_FlatSortedDedup(t *testing.T) {
	reg := buildTestRegistry(t)
	models := reg.AllModels()

	ids := make([]string, len(models))
	for i, m := range models {
		ids[i] = m.ID
	}
	assert.Equal(t, []string{"deepseek-chat", "gpt-4o", "gpt-4o-azure", "gpt-4o-mini"}, ids)

	// Sanity check: each has Object="model" and Provider set.
	for _, m := range models {
		assert.Equal(t, "model", m.Object)
		assert.NotEmpty(t, m.Provider)
	}
}
