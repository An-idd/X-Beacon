package server

import (
	"encoding/json"
	"net/http"

	"github.com/prometheus/client_golang/prometheus"
	"go.uber.org/zap"

	"github.com/An-idd/x-beacon/internal/route"
	"github.com/An-idd/x-beacon/internal/server/middleware"
)

// adminRoutingRulesHandler returns the smart-routing rules currently
// loaded into the classifier, joined with each rule's hit count read
// out of the gateway's Prometheus counter
// (`gateway_router_decision_total{rule="..."}`).
//
// Read-only by design — config-side editing waits for a hot-reload
// mechanism (Phase 5+). Operators still update rules by editing
// configs/config.yaml and restarting; this endpoint just exposes
// "what's currently active" for the WebUI.
//
// classifier may be nil (smart routing disabled). The handler then
// returns an empty list with a flag so the WebUI can show an
// informational empty-state instead of a confusing zero-row table.
func adminRoutingRulesHandler(classifier route.Classifier, gatherer prometheus.Gatherer, logger *zap.Logger) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		reqID := middleware.RequestIDFrom(r.Context())

		hits, err := gatherRouteHits(gatherer)
		if err != nil {
			logger.Warn("admin routing: gather hits failed; rules will report 0",
				zap.String("req_id", reqID), zap.Error(err))
			hits = map[string]uint64{} // degrade to zero-counts, not 500
		}

		var dtos []routingRuleDTO
		var enabled bool
		if rc, ok := classifier.(*route.RuleClassifier); ok && rc != nil {
			enabled = true
			rules := rc.Rules()
			dtos = make([]routingRuleDTO, 0, len(rules))
			for _, rule := range rules {
				dtos = append(dtos, routingRuleDTO{
					Name:    rule.Name,
					RouteTo: rule.RouteTo,
					When: routingConditionDTO{
						MaxTokens:    rule.When.MaxTokens,
						MinTokens:    rule.When.MinTokens,
						KeywordsAny:  rule.When.KeywordsAny,
						KeywordsNone: rule.When.KeywordsNone,
					},
					Hits: hits[rule.Name],
				})
			}
		}

		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"enabled": enabled,
			"rules":   dtos,
		})
	}
}

type routingRuleDTO struct {
	Name    string              `json:"name"`
	RouteTo string              `json:"route_to"`
	When    routingConditionDTO `json:"when"`
	Hits    uint64              `json:"hits"`
}

type routingConditionDTO struct {
	MaxTokens    int      `json:"max_tokens,omitempty"`
	MinTokens    int      `json:"min_tokens,omitempty"`
	KeywordsAny  []string `json:"keywords_any,omitempty"`
	KeywordsNone []string `json:"keywords_none,omitempty"`
}

// gatherRouteHits extracts the rule → counter map from the registry.
// Walks gateway_router_decision_total once and projects to {rule: sum}
// (collapsing the from / to dimensions; the WebUI only displays per-
// rule counts, which is the most useful comparison anyway).
func gatherRouteHits(g prometheus.Gatherer) (map[string]uint64, error) {
	out := map[string]uint64{}
	if g == nil {
		return out, nil
	}
	families, err := g.Gather()
	if err != nil {
		return nil, err
	}
	for _, f := range families {
		if f.GetName() != "gateway_router_decision_total" {
			continue
		}
		for _, m := range f.GetMetric() {
			rule := ""
			for _, lp := range m.GetLabel() {
				if lp.GetName() == "rule" {
					rule = lp.GetValue()
					break
				}
			}
			if rule == "" {
				continue
			}
			out[rule] += uint64(m.GetCounter().GetValue())
		}
	}
	return out, nil
}
