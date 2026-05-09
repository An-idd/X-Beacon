package server

import (
	"encoding/json"
	"net/http"
	"sort"

	"github.com/prometheus/client_golang/prometheus"
	dto "github.com/prometheus/client_model/go"
	"go.uber.org/zap"

	"github.com/An-idd/x-beacon/internal/provider/registry"
	"github.com/An-idd/x-beacon/internal/server/middleware"
)

// adminProvidersHandler exposes the registry plus runtime health
// (circuit breaker state, failover counters) so an admin can answer
// "is my backup chain working" without grepping `/metrics`.
//
// One handler joins three sources:
//
//   - registry.Names() — declaration order; primary axis
//   - registry.AllModels() — model → owned_by mapping (collapsed to
//     "this provider serves these models")
//   - gateway_breaker_state{provider} gauge (closed/half/open)
//   - gateway_router_failover_total{from, to} counter
//
// Output is intentionally a flat array of providers, one row per
// provider, with breaker state + outbound failover counts on the
// row. Inbound failovers (other providers giving traffic to this
// one) are summarized in a separate field for completeness.
func adminProvidersHandler(reg *registry.Registry, gatherer prometheus.Gatherer, logger *zap.Logger) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		reqID := middleware.RequestIDFrom(r.Context())

		breakerByProvider := map[string]int{}
		failoverFrom := map[string]uint64{} // sum over `from`
		failoverTo := map[string]uint64{}   // sum over `to`

		if gatherer != nil {
			families, err := gatherer.Gather()
			if err != nil {
				logger.Warn("admin providers: gather failed; runtime health columns will be empty",
					zap.String("req_id", reqID), zap.Error(err))
			}
			for _, f := range families {
				switch f.GetName() {
				case "gateway_breaker_state":
					for _, m := range f.GetMetric() {
						p := labelValueDirect(m.GetLabel(), "provider")
						if p == "" {
							continue
						}
						breakerByProvider[p] = int(m.GetGauge().GetValue())
					}
				case "gateway_router_failover_total":
					for _, m := range f.GetMetric() {
						from := labelValueDirect(m.GetLabel(), "from")
						to := labelValueDirect(m.GetLabel(), "to")
						v := uint64(m.GetCounter().GetValue())
						if from != "" {
							failoverFrom[from] += v
						}
						if to != "" {
							failoverTo[to] += v
						}
					}
				}
			}
		}

		// Build models-by-provider via the OwnedBy field. Same
		// convention OpenAI's /v1/models uses; AllModels() preserves
		// it.
		modelsByOwner := map[string][]string{}
		for _, m := range reg.AllModels() {
			if m.OwnedBy == "" {
				continue
			}
			modelsByOwner[m.OwnedBy] = append(modelsByOwner[m.OwnedBy], m.ID)
		}
		for k := range modelsByOwner {
			sort.Strings(modelsByOwner[k])
		}

		names := reg.Names()
		out := make([]providerDTO, 0, len(names))
		for _, name := range names {
			out = append(out, providerDTO{
				Name:           name,
				Models:         modelsByOwner[name],
				BreakerState:   breakerStateLabel(breakerByProvider[name]),
				FailoversFrom:  failoverFrom[name],
				FailoversTo:    failoverTo[name],
			})
		}

		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"providers": out,
		})
	}
}

type providerDTO struct {
	Name string `json:"name"`
	// Models that name this provider as their `owned_by`. May be
	// empty if the provider catalog hasn't propagated owned_by
	// (older configs).
	Models []string `json:"models"`
	// BreakerState is one of "closed", "half_open", "open", or
	// "unknown" when the breaker hasn't reported yet.
	BreakerState string `json:"breaker_state"`
	// FailoversFrom counts requests that started here and got handed
	// off to a backup. Spike = this provider is degrading.
	FailoversFrom uint64 `json:"failovers_from"`
	// FailoversTo counts requests handed TO this provider as backup.
	// Useful for verifying chain configuration (a brand-new "backup"
	// row staying at 0 means it's never been exercised).
	FailoversTo uint64 `json:"failovers_to"`
}

// breakerStateLabel maps the integer gauge values from
// observability.SetBreakerState (0/1/2) to human-readable strings
// the WebUI can render directly. "unknown" covers the not-yet-reported
// case so the table's first paint isn't blank.
func breakerStateLabel(state int) string {
	switch state {
	case 0:
		return "closed"
	case 1:
		return "half_open"
	case 2:
		return "open"
	default:
		// SetBreakerState is called on every state transition, so a
		// provider stays "unknown" only until its first
		// success/failure. After that the row reflects truth.
		return "unknown"
	}
}

// labelValueDirect is the same idea as observability.labelValue but
// works on a label slice we already have on hand — avoids re-walking
// the wrapping *dto.Metric.
func labelValueDirect(labels []*dto.LabelPair, name string) string {
	for _, lp := range labels {
		if lp.GetName() == name {
			return lp.GetValue()
		}
	}
	return ""
}
