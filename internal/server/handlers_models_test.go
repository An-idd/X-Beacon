package server

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/An-idd/x-beacon/internal/auth"
	"github.com/An-idd/x-beacon/internal/billing"
	"github.com/An-idd/x-beacon/internal/observability"
	"github.com/An-idd/x-beacon/internal/provider"
	"github.com/An-idd/x-beacon/internal/provider/openai"
	"github.com/An-idd/x-beacon/internal/provider/registry"
)

// stubPricing implements pricingLookup for hermetic tests. The real
// billing.PricingCache requires a *storage.Pool to seed; stubbing the
// interface keeps the test in-process and free of fixtures.
type stubPricing struct {
	rates map[string]billing.Rate
}

func (s *stubPricing) Lookup(model string) (billing.Rate, bool) {
	r, ok := s.rates[model]
	return r, ok
}

// fakeRegistry builds a real registry with three providers carrying a
// mix of "catalog-known" model IDs (gpt-4o, claude-3-5-sonnet-20241022,
// deepseek-chat) and one "off-catalog" ID (bespoke-model-xyz) so we can
// assert both enrichment paths in one fixture.
func fakeRegistryForModels(t *testing.T) *registry.Registry {
	t.Helper()
	openaiP, err := openai.New(openai.Config{
		Name:   "openai-primary",
		APIKey: "sk-x",
		Models: openai.Models{Exact: []string{"gpt-4o", "bespoke-model-xyz"}},
	})
	require.NoError(t, err)

	// Use openai adapter for all three to avoid Anthropic/DeepSeek
	// constructor variance — the handler only cares about the
	// SupportedModels() shape (ID + OwnedBy + Provider), not the
	// underlying transport.
	anthropicP, err := openai.New(openai.Config{
		Name:    "anthropic-primary",
		APIKey:  "sk-y",
		OwnedBy: "anthropic",
		Models:  openai.Models{Exact: []string{"claude-3-5-sonnet-20241022"}},
	})
	require.NoError(t, err)

	deepseekP, err := openai.New(openai.Config{
		Name:    "deepseek",
		APIKey:  "sk-d",
		OwnedBy: "deepseek",
		Models:  openai.Models{Exact: []string{"deepseek-chat"}},
	})
	require.NoError(t, err)

	// White-box construction — same pattern registry/registry_test.go
	// uses for focused tests that bypass YAML loading.
	return buildRegistryFromProviders(t, openaiP, anthropicP, deepseekP)
}

// buildRegistryFromProviders is a thin wrapper that uses registry.Load
// indirectly via a tiny in-memory YAML so we don't need to export the
// internal Registry constructor. We assemble providers via the public
// helper if available; otherwise this falls back to YAML round-trip.
//
// Implementation note: the registry package keeps its struct fields
// unexported, so the cleanest cross-package fixture is to build a
// minimal Registry via the exported Load API using a temp file. To
// avoid the file dance in unit tests we instead use the openai
// adapter's SupportedModels() output and wrap it in an empty
// registry seeded via a custom helper exported only for tests.
// Since registry doesn't expose such a helper today, we round-trip
// through Load on a temp file.
func buildRegistryFromProviders(t *testing.T, ps ...provider.Provider) *registry.Registry {
	t.Helper()
	// The registry only exposes NewEmpty + Load. NewEmpty has no
	// providers (handler renders []). For tests asserting enrichment
	// we need a populated registry. Use Load with a tempfile.
	yaml := "providers:\n"
	for _, p := range ps {
		models := p.SupportedModels()
		if len(models) == 0 {
			continue
		}
		ptype := "openai" // all fixtures use openai adapter underneath
		yaml += "  - name: " + p.Name() + "\n"
		yaml += "    type: " + ptype + "\n"
		yaml += "    api_key: dummy\n"
		yaml += "    models:\n      exact:\n"
		for _, m := range models {
			yaml += "        - " + m.ID + "\n"
		}
	}
	yaml += "default_provider: " + ps[0].Name() + "\n"

	dir := t.TempDir()
	path := dir + "/providers.yaml"
	require.NoError(t, writeFile(path, yaml))
	reg, err := registry.Load(path)
	require.NoError(t, err)
	return reg
}

// writeFile is a tiny helper that wraps os.WriteFile so we keep the
// import-and-mode boilerplate out of the fixture builder above.
func writeFile(path, content string) error {
	return os.WriteFile(path, []byte(content), 0o600)
}

// ---------- tests ----------

func TestModels_EmptyRegistryReturnsEmptyData(t *testing.T) {
	reg := registry.NewEmpty()
	h := modelsHandler(reg, nil, nil)
	req := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
	rec := httptest.NewRecorder()
	h(rec, req)

	require.Equal(t, http.StatusOK, rec.Code)
	var body modelsResponse
	require.NoError(t, json.NewDecoder(rec.Body).Decode(&body))
	assert.Equal(t, "list", body.Object)
	assert.NotNil(t, body.Data) // must serialize as [] not null
	assert.Empty(t, body.Data)
}

func TestModels_KnownCatalogModelCarriesFullMetadata(t *testing.T) {
	reg := fakeRegistryForModels(t)
	h := modelsHandler(reg, nil, nil)
	req := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
	rec := httptest.NewRecorder()
	h(rec, req)

	require.Equal(t, http.StatusOK, rec.Code)
	gpt := findModel(t, rec.Body.Bytes(), "gpt-4o")
	assert.Equal(t, 128_000, gpt.ContextLength)
	assert.NotEmpty(t, gpt.Capabilities)
	assert.Contains(t, gpt.Capabilities, provider.CapabilityVision)
	require.NotNil(t, gpt.DataPolicy)
	assert.Equal(t, "opt_out", gpt.DataPolicy.Training)
	require.NotNil(t, gpt.Pricing)
	assert.Equal(t, "0.0025", gpt.Pricing.Prompt)
	assert.Equal(t, "0.01", gpt.Pricing.Completion)
	assert.Equal(t, "USD", gpt.Pricing.Currency)
	assert.Equal(t, "1K_tokens", gpt.Pricing.Unit)
}

func TestModels_UnknownCatalogModelOmitsExtensionFields(t *testing.T) {
	// Backward compatibility: a model not in the catalog should appear
	// in /v1/models with the OpenAI base shape only, no surprise null
	// fields injected.
	reg := fakeRegistryForModels(t)
	h := modelsHandler(reg, nil, nil)
	req := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
	rec := httptest.NewRecorder()
	h(rec, req)

	bespoke := findModel(t, rec.Body.Bytes(), "bespoke-model-xyz")
	assert.Equal(t, "model", bespoke.Object)
	assert.Equal(t, 0, bespoke.ContextLength)
	assert.Empty(t, bespoke.Capabilities)
	assert.Nil(t, bespoke.DataPolicy)
	assert.Nil(t, bespoke.Pricing)
	assert.Empty(t, bespoke.Status)
}

func TestModels_OffCatalogModelHasNoPricingField(t *testing.T) {
	// Stronger assertion than the prior test: the JSON should not even
	// contain a "pricing" key for off-catalog models, since omitempty
	// must elide nil pointers (verifies wire shape, not just struct).
	reg := fakeRegistryForModels(t)
	h := modelsHandler(reg, nil, nil)
	req := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
	rec := httptest.NewRecorder()
	h(rec, req)

	var envelope struct {
		Data []map[string]any `json:"data"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &envelope))
	for _, m := range envelope.Data {
		if m["id"] == "bespoke-model-xyz" {
			_, hasPricing := m["pricing"]
			assert.False(t, hasPricing, "off-catalog model must not have pricing key in JSON")
			_, hasStatus := m["status"]
			assert.False(t, hasStatus, "off-catalog model must not have status key when no admin scope")
			return
		}
	}
	t.Fatal("bespoke-model-xyz not found in response")
}

func TestModels_PricingCacheOverridesCatalogDefault(t *testing.T) {
	reg := fakeRegistryForModels(t)
	// Override gpt-4o's catalog default ($2.50/$10 per 1M) with a
	// fictitious admin-set price. The wire response must reflect the
	// admin value.
	stub := &stubPricing{rates: map[string]billing.Rate{
		"gpt-4o": {Model: "gpt-4o", Currency: "USD", InputPer1kMicro: 1234, OutputPer1kMicro: 5678},
	}}
	h := modelsHandler(reg, stub, nil)
	req := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
	rec := httptest.NewRecorder()
	h(rec, req)

	gpt := findModel(t, rec.Body.Bytes(), "gpt-4o")
	require.NotNil(t, gpt.Pricing)
	assert.Equal(t, "0.001234", gpt.Pricing.Prompt)
	assert.Equal(t, "0.005678", gpt.Pricing.Completion)
}

func TestModels_PricingCacheZeroDoesNotOverrideCatalog(t *testing.T) {
	// A pricing-cache row with zero rates should be treated as "no
	// price published" and NOT clobber the catalog default. This
	// matches FormatPricing's "zero → nil" contract.
	reg := fakeRegistryForModels(t)
	stub := &stubPricing{rates: map[string]billing.Rate{
		"gpt-4o": {Model: "gpt-4o", Currency: "USD", InputPer1kMicro: 0, OutputPer1kMicro: 0},
	}}
	h := modelsHandler(reg, stub, nil)
	req := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
	rec := httptest.NewRecorder()
	h(rec, req)

	gpt := findModel(t, rec.Body.Bytes(), "gpt-4o")
	require.NotNil(t, gpt.Pricing)
	assert.Equal(t, "0.0025", gpt.Pricing.Prompt) // catalog default
}

func TestModels_StatusFieldRequiresAdminScope(t *testing.T) {
	reg := fakeRegistryForModels(t)
	gatherer := gathererWithBreakerStates(t, map[string]float64{
		"openai-primary": 2, // open → unavailable
	})

	// Anonymous (no Principal). Status must be omitted.
	t.Run("anonymous_no_status", func(t *testing.T) {
		h := modelsHandler(reg, nil, gatherer)
		req := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
		rec := httptest.NewRecorder()
		h(rec, req)
		gpt := findModel(t, rec.Body.Bytes(), "gpt-4o")
		assert.Empty(t, gpt.Status)
	})

	// Non-admin scope. Status must be omitted.
	t.Run("non_admin_no_status", func(t *testing.T) {
		h := modelsHandler(reg, nil, gatherer)
		p := &auth.Principal{ID: "k1", Scopes: map[string][]string{"rate": {"high"}}}
		req := httptest.NewRequest(http.MethodGet, "/v1/models", nil).
			WithContext(auth.WithPrincipal(context.Background(), p))
		rec := httptest.NewRecorder()
		h(rec, req)
		gpt := findModel(t, rec.Body.Bytes(), "gpt-4o")
		assert.Empty(t, gpt.Status)
	})

	// admin:webui present. Status must reflect breaker state.
	t.Run("admin_scope_shows_status", func(t *testing.T) {
		h := modelsHandler(reg, nil, gatherer)
		p := &auth.Principal{ID: "admin1", Scopes: map[string][]string{"admin": {"webui"}}}
		req := httptest.NewRequest(http.MethodGet, "/v1/models", nil).
			WithContext(auth.WithPrincipal(context.Background(), p))
		rec := httptest.NewRecorder()
		h(rec, req)
		gpt := findModel(t, rec.Body.Bytes(), "gpt-4o")
		assert.Equal(t, "unavailable", gpt.Status)
	})
}

func TestModels_BreakerStateMapping(t *testing.T) {
	reg := fakeRegistryForModels(t)
	cases := []struct {
		state float64
		want  string
	}{
		{0, "available"},
		{1, "degraded"},
		{2, "unavailable"},
		{99, "unavailable"}, // unknown values are conservative
	}
	for _, tc := range cases {
		t.Run(tc.want, func(t *testing.T) {
			g := gathererWithBreakerStates(t, map[string]float64{
				"openai-primary": tc.state,
			})
			h := modelsHandler(reg, nil, g)
			p := &auth.Principal{ID: "admin1", Scopes: map[string][]string{"admin": {"webui"}}}
			req := httptest.NewRequest(http.MethodGet, "/v1/models", nil).
				WithContext(auth.WithPrincipal(context.Background(), p))
			rec := httptest.NewRecorder()
			h(rec, req)
			gpt := findModel(t, rec.Body.Bytes(), "gpt-4o")
			assert.Equal(t, tc.want, gpt.Status)
		})
	}
}

func TestModels_NilDepsDoNotPanic(t *testing.T) {
	reg := fakeRegistryForModels(t)
	h := modelsHandler(reg, nil, nil)
	req := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
	rec := httptest.NewRecorder()
	require.NotPanics(t, func() { h(rec, req) })
	assert.Equal(t, http.StatusOK, rec.Code)
}

func TestModels_ResponseEnvelopeShape(t *testing.T) {
	// Belt-and-suspenders: the top-level shape must be `{"object":
	// "list", "data": [...]}` — what every OpenAI SDK looks for.
	reg := fakeRegistryForModels(t)
	h := modelsHandler(reg, nil, nil)
	req := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
	rec := httptest.NewRecorder()
	h(rec, req)

	var raw map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &raw))
	assert.Equal(t, "list", raw["object"])
	_, hasData := raw["data"].([]any)
	assert.True(t, hasData, "data must be a JSON array")
}

// ---------- helpers ----------

// findModel decodes the response body and returns the entry matching id,
// failing the test if not found. Returns by value so subtests can
// modify their copy without bleeding state.
func findModel(t *testing.T, body []byte, id string) provider.ModelInfo {
	t.Helper()
	var resp modelsResponse
	require.NoError(t, json.Unmarshal(body, &resp))
	for _, m := range resp.Data {
		if m.ID == id {
			return m
		}
	}
	t.Fatalf("model %q not found in /v1/models response", id)
	return provider.ModelInfo{}
}

// gathererWithBreakerStates seeds an isolated Prometheus registry with
// breaker state gauges and returns it as a Gatherer. Uses the real
// observability.Metrics so the metric family name + label conventions
// stay in lockstep with production wiring.
func gathererWithBreakerStates(t *testing.T, byProvider map[string]float64) prometheus.Gatherer {
	t.Helper()
	reg := prometheus.NewRegistry()
	m, err := observability.NewMetrics(reg)
	require.NoError(t, err)
	for provName, state := range byProvider {
		m.SetBreakerState(provName, int(state))
	}
	return reg
}
