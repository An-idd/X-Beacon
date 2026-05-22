package catalog

import (
	"testing"

	"github.com/An-idd/x-beacon/internal/provider"
)

func TestLookup_KnownModelHasCompleteData(t *testing.T) {
	e, ok := Lookup("gpt-4o")
	if !ok {
		t.Fatal("gpt-4o should be in builtin catalog")
	}
	if e.ContextLength != 128_000 {
		t.Errorf("ContextLength = %d, want 128000", e.ContextLength)
	}
	if len(e.Capabilities) == 0 {
		t.Error("Capabilities must not be empty for gpt-4o")
	}
	if e.DataPolicy == nil || e.DataPolicy.Training != "opt_out" {
		t.Errorf("DataPolicy = %+v, want training=opt_out", e.DataPolicy)
	}
	if e.DefaultPromptPer1kMicro == 0 || e.DefaultCompletionPer1kMicro == 0 {
		t.Errorf("default pricing missing: prompt=%d completion=%d",
			e.DefaultPromptPer1kMicro, e.DefaultCompletionPer1kMicro)
	}
}

func TestLookup_UnknownModelReturnsFalse(t *testing.T) {
	if e, ok := Lookup("definitely-not-a-model"); ok {
		t.Errorf("Lookup unknown model returned ok=true, entry=%+v", e)
	}
}

func TestBuiltin_AllCoreProvidersCovered(t *testing.T) {
	// The 3 already-shipped adapters must each have at least one
	// flagship model in the catalog so /v1/models is non-empty out of
	// the box for the example providers.yaml.
	wantSome := []string{
		"gpt-4o",
		"claude-3-5-sonnet-20241022",
		"deepseek-chat",
	}
	for _, id := range wantSome {
		if _, ok := builtin[id]; !ok {
			t.Errorf("missing flagship entry: %s", id)
		}
	}
}

func TestFormatPricing_DecimalRoundtripPrecision(t *testing.T) {
	// The whole reason we serialize as strings: $0.00014 / 1k must
	// survive intact and not become "0.00013999..." or similar.
	cases := []struct {
		name           string
		prompt         int64
		completion     int64
		wantPrompt     string
		wantCompletion string
	}{
		{"deepseek-chat", 140, 280, "0.00014", "0.00028"},
		{"gpt-4o-mini", 150, 600, "0.00015", "0.0006"},
		{"gpt-4o", 2_500, 10_000, "0.0025", "0.01"},
		{"claude-opus", 15_000, 75_000, "0.015", "0.075"},
		{"hypothetical-whole-dollar", 1_000_000, 2_000_000, "1.0", "2.0"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p := FormatPricing(tc.prompt, tc.completion, "USD")
			if p == nil {
				t.Fatal("FormatPricing returned nil for valid input")
			}
			if p.Prompt != tc.wantPrompt {
				t.Errorf("Prompt = %q, want %q", p.Prompt, tc.wantPrompt)
			}
			if p.Completion != tc.wantCompletion {
				t.Errorf("Completion = %q, want %q", p.Completion, tc.wantCompletion)
			}
			if p.Currency != "USD" {
				t.Errorf("Currency = %q, want USD", p.Currency)
			}
			if p.Unit != "1K_tokens" {
				t.Errorf("Unit = %q, want 1K_tokens", p.Unit)
			}
		})
	}
}

func TestFormatPricing_ZeroOrNegativeReturnsNil(t *testing.T) {
	cases := [][2]int64{
		{0, 100},
		{100, 0},
		{0, 0},
		{-1, 100},
	}
	for _, c := range cases {
		if p := FormatPricing(c[0], c[1], "USD"); p != nil {
			t.Errorf("FormatPricing(%d, %d) = %+v, want nil", c[0], c[1], p)
		}
	}
}

func TestFormatPricing_DefaultCurrency(t *testing.T) {
	p := FormatPricing(100, 200, "")
	if p == nil || p.Currency != "USD" {
		t.Errorf("empty currency should default to USD, got %+v", p)
	}
}

func TestModelPricingShape(t *testing.T) {
	// Defense in depth: catch accidental ModelPricing rename in the
	// provider package that would silently break wire compatibility.
	var _ provider.ModelPricing = provider.ModelPricing{
		Prompt:     "x",
		Completion: "y",
		Currency:   "USD",
		Unit:       "1K_tokens",
	}
}
