package server

import (
	"encoding/json"
	"net/http"

	"github.com/prometheus/client_golang/prometheus"
	dto "github.com/prometheus/client_model/go"

	"github.com/An-idd/x-beacon/internal/auth"
	"github.com/An-idd/x-beacon/internal/billing"
	"github.com/An-idd/x-beacon/internal/provider"
	"github.com/An-idd/x-beacon/internal/provider/catalog"
	"github.com/An-idd/x-beacon/internal/provider/registry"
)

// pricingLookup is the narrow surface modelsHandler needs out of
// billing.PricingCache. Defined here (consumer side, per Go idiom) so
// tests can inject a stub without spinning up a real DB pool.
// *billing.PricingCache satisfies this implicitly.
type pricingLookup interface {
	Lookup(model string) (billing.Rate, bool)
}

// modelsResponse mirrors the OpenAI /v1/models response envelope.
type modelsResponse struct {
	Object string               `json:"object"`
	Data   []provider.ModelInfo `json:"data"`
}

// modelsHandler returns a handler for GET /v1/models. The response format
// is OpenAI-compatible at the envelope level so existing SDKs discover the
// gateway's catalog without adaptation; X-Beacon enriches each entry with
// `pricing`, `context_length`, `capabilities`, and `data_policy` from the
// static catalog and the live pricing snapshot. The `status` field, which
// surfaces circuit-breaker state, is gated on the admin:webui scope so
// regular keys cannot probe per-provider liveness.
//
// Both `pricing` (PricingCache) and `gatherer` are optional; when either is
// nil the corresponding fields are silently omitted, which keeps the
// endpoint useful in dev mode without DB or metrics scrape targets.
func modelsHandler(reg *registry.Registry, pricing pricingLookup, gatherer prometheus.Gatherer) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		base := reg.AllModels()

		// Snapshot breaker state once per request. Empty map is fine —
		// enrichModel treats "not present" as "omit status field".
		breakerByProvider := gatherBreakerStates(gatherer)

		// admin:webui scope unlocks the `status` field. Convention
		// matches Phase 4 endpoints (admin_providers, admin_logs, etc.).
		showStatus := auth.PrincipalFrom(r.Context()).HasScope("admin", "webui")

		enriched := make([]provider.ModelInfo, 0, len(base))
		for _, m := range base {
			enriched = append(enriched, enrichModel(m, pricing, breakerByProvider, showStatus))
		}

		resp := modelsResponse{Object: "list", Data: enriched}
		// Ensure non-nil slice so JSON renders `[]` instead of `null`.
		if resp.Data == nil {
			resp.Data = []provider.ModelInfo{}
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(resp)
	}
}

// enrichModel folds catalog + pricing + breaker data into a base ModelInfo.
// The base ModelInfo comes from the adapter's SupportedModels() and carries
// only the OpenAI-canonical fields (ID, Object, OwnedBy) plus the
// non-serialized Provider name. Everything else is layered on here.
//
// Layering rules:
//   - capabilities / context_length / data_policy: come from the catalog
//     only. Absent in catalog → field omitted from response.
//   - pricing: pricing cache (admin-set DB rows) wins over catalog default.
//     Both missing → field omitted (better than misleading "$0.00").
//   - status: only emitted when showStatus is true AND the breaker has
//     reported. Maps gauge values 0/1/2 → available/degraded/unavailable.
func enrichModel(
	base provider.ModelInfo,
	pricing pricingLookup,
	breakerByProvider map[string]int,
	showStatus bool,
) provider.ModelInfo {
	out := base
	if entry, ok := catalog.Lookup(base.ID); ok {
		out.ContextLength = entry.ContextLength
		out.Capabilities = entry.Capabilities
		out.DataPolicy = entry.DataPolicy
		out.Pricing = catalog.FormatPricing(
			entry.DefaultPromptPer1kMicro,
			entry.DefaultCompletionPer1kMicro,
			entry.DefaultCurrency,
		)
	}

	// Pricing cache (admin API / model_pricing table) overrides the
	// catalog default. Lookup returns (Rate{}, false) when the model
	// isn't priced; we keep whatever the catalog gave us in that case.
	if pricing != nil {
		if rate, ok := pricing.Lookup(base.ID); ok {
			if p := catalog.FormatPricing(rate.InputPer1kMicro, rate.OutputPer1kMicro, rate.Currency); p != nil {
				out.Pricing = p
			}
		}
	}

	// Status is the optional, scoped field. We only attach it when the
	// caller is authorized AND the breaker for the model's owning
	// provider has reported at least once — "unknown" status would be
	// noise for unauthenticated dev-mode use.
	if showStatus {
		if state, ok := breakerByProvider[base.Provider]; ok {
			out.Status = statusLabelForBreakerState(state)
		}
	}

	return out
}

// statusLabelForBreakerState maps the integer values emitted by
// observability.SetBreakerState (0=closed, 1=half_open, 2=open) onto the
// /v1/models `status` enum (available | degraded | unavailable). Any
// unexpected value is reported as "unavailable" — better safe than
// optimistic for a value that influences client routing decisions.
func statusLabelForBreakerState(state int) string {
	switch state {
	case 0:
		return "available"
	case 1:
		return "degraded"
	default:
		return "unavailable"
	}
}

// gatherBreakerStates pulls the latest gateway_breaker_state{provider}
// gauge values into a map. Returns an empty (non-nil) map when the
// gatherer is nil or the gauge family hasn't been registered, so callers
// can index without nil checks.
func gatherBreakerStates(g prometheus.Gatherer) map[string]int {
	out := map[string]int{}
	if g == nil {
		return out
	}
	families, err := g.Gather()
	if err != nil {
		return out
	}
	for _, f := range families {
		if f.GetName() != "gateway_breaker_state" {
			continue
		}
		for _, m := range f.GetMetric() {
			provider := labelValueOnGauge(m.GetLabel(), "provider")
			if provider == "" {
				continue
			}
			out[provider] = int(m.GetGauge().GetValue())
		}
	}
	return out
}

// labelValueOnGauge scans the label slice for a name match. Inlined here
// rather than importing the admin_providers helper to keep the
// /v1/models path independent of /admin code paths.
func labelValueOnGauge(labels []*dto.LabelPair, name string) string {
	for _, lp := range labels {
		if lp.GetName() == name {
			return lp.GetValue()
		}
	}
	return ""
}

// healthzHandler is a minimal liveness probe. Kept stupid on purpose: any
// dependency check belongs in /readyz (Week 4).
func healthzHandler() http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok\n"))
	}
}
