package server

import (
	"encoding/json"
	"net/http"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"go.uber.org/zap"

	"github.com/An-idd/x-beacon/internal/config"
	"github.com/An-idd/x-beacon/internal/server/middleware"
)

// adminRateLimitRulesHandler exposes the configured rate-limit rules
// joined with each rule's reject count
// (`gateway_ratelimit_rejected_total{rule}`).
//
// Read-only. Edits go through configs/config.yaml + restart, same
// as routing rules. The rules slice is the parsed YAML form; it
// includes algorithm + rate/window/limit so the WebUI can show
// "100/s memory bucket" without parsing a string spec.
func adminRateLimitRulesHandler(rules []config.RateLimitRule, gatherer prometheus.Gatherer, logger *zap.Logger) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		reqID := middleware.RequestIDFrom(r.Context())

		rejects, err := gatherRatelimitRejects(gatherer)
		if err != nil {
			logger.Warn("admin ratelimit: gather rejects failed; counts will be 0",
				zap.String("req_id", reqID), zap.Error(err))
			rejects = map[string]uint64{}
		}

		out := make([]ratelimitRuleDTO, 0, len(rules))
		for _, rl := range rules {
			out = append(out, ratelimitRuleDTO{
				Name:      rl.Name,
				Algorithm: rl.Algorithm,
				Rate:      rl.Rate,
				Window:    durString(rl.Window),
				Limit:     rl.Limit,
				Burst:     rl.Burst,
				KeyBy:     rl.KeyBy,
				Rejects:   rejects[rl.Name],
			})
		}

		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"enabled": len(rules) > 0,
			"rules":   out,
		})
	}
}

type ratelimitRuleDTO struct {
	Name      string   `json:"name"`
	Algorithm string   `json:"algorithm"` // memory_bucket | redis_window
	// Algorithm-specific spec fields. Empty/zero when not applicable
	// to the chosen algorithm — keeps the wire shape flat instead of
	// branching on `algorithm` to two different sub-structs.
	Rate    string   `json:"rate,omitempty"`    // memory_bucket: "100/s"
	Window  string   `json:"window,omitempty"`  // redis_window: "1m"
	Limit   int      `json:"limit,omitempty"`   // redis_window: 100
	Burst   int      `json:"burst,omitempty"`   // memory_bucket: optional
	KeyBy   []string `json:"key_by"`            // [] | [api_key] | [api_key, model]
	// Rejects is the cumulative reject count from
	// gateway_ratelimit_rejected_total{rule=<name>}.
	Rejects uint64 `json:"rejects"`
}

// durString renders a time.Duration in compact form (e.g. "1m"),
// or "" for the zero duration. Avoids the verbose
// time.Duration.String() output ("1m0s").
func durString(d time.Duration) string {
	if d == 0 {
		return ""
	}
	return d.String()
}

// gatherRatelimitRejects walks gateway_ratelimit_rejected_total once
// and projects to {rule: count}.
func gatherRatelimitRejects(g prometheus.Gatherer) (map[string]uint64, error) {
	out := map[string]uint64{}
	if g == nil {
		return out, nil
	}
	families, err := g.Gather()
	if err != nil {
		return nil, err
	}
	for _, f := range families {
		if f.GetName() != "gateway_ratelimit_rejected_total" {
			continue
		}
		for _, m := range f.GetMetric() {
			rule := labelValueDirect(m.GetLabel(), "rule")
			if rule == "" {
				continue
			}
			out[rule] += uint64(m.GetCounter().GetValue())
		}
	}
	return out, nil
}
