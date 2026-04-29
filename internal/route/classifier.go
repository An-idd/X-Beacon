// Package route holds the smart-routing layer that picks an upstream
// model based on prompt characteristics. The motivation is cost: a
// "translate this paragraph" request can be served by a $0.15/1M-tok
// model where a "explain this distributed-system bug" request needs
// a $5.00/1M-tok one.
//
// Week 11 ships only the rule-engine implementation: token-count +
// keyword-whitelist conditions, evaluated in order, first match wins.
// Anything fancier (statistical classifier, LLM-as-judge) is
// deliberately out of scope per CLAUDE.md L156 — "不做 ML 分类器,
// 除非数据证明需要".
package route

import (
	"github.com/An-idd/x-beacon/internal/provider"
)

// Decision is what Classify returns: the target model + the matched
// rule name. An empty Model means "leave the request's model
// unchanged" — the router will fall back to whatever the client
// asked for.
type Decision struct {
	// Model is the target upstream model id. Empty when no rule
	// matched (caller treats as "use req.Model verbatim").
	Model string

	// Rule is the matched rule's name; "" when Model == "". Surfaced
	// in metrics + traces so ops can attribute traffic to specific
	// rules without inspecting the YAML.
	Rule string
}

// Empty reports whether the decision is a no-op. Callers use this to
// short-circuit metric updates and avoid emitting "rerouted" labels
// for the default-model path.
func (d Decision) Empty() bool { return d.Model == "" }

// Classifier picks a target model from a chat request. Implementations
// must be safe for concurrent use; Classify is pure (no ctx, no IO)
// because rule evaluation runs on every request and mustn't block.
type Classifier interface {
	Classify(req *provider.ChatRequest) Decision
}
