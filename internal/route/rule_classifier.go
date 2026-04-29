package route

import (
	"errors"
	"fmt"
	"strings"

	"github.com/An-idd/x-beacon/internal/provider"
	"github.com/An-idd/x-beacon/pkg/tokenizer"
)

// Rule is one entry in the routing.rules YAML block. Evaluated
// top-to-bottom; first match wins. Routing decisions never cascade —
// a request that matches the second rule does not also apply the
// fifth.
type Rule struct {
	// Name is the human-readable id used in metrics
	// (`gateway_router_decision_total{reason="rule:<name>"}`).
	// Required, must be unique within a Classifier.
	Name string

	// RouteTo is the target model id. Required. The router will
	// resolve this through the same registry it would have used for
	// the original model — so RouteTo must point at a configured
	// upstream model.
	RouteTo string

	// When is the AND-joined condition set. Empty conditions on a
	// rule make it match every request (acts as a catch-all when
	// placed last; see the example config for the idiom).
	When Condition
}

// Condition is the matchable predicate. Multiple fields AND together;
// keyword lists OR within themselves. Zero values mean "don't check".
type Condition struct {
	// MaxTokens matches when the request's prompt-token count is
	// less than or equal to this value. 0 = unbounded.
	MaxTokens int

	// MinTokens matches when prompt-tokens >= this value. 0 means
	// no lower bound.
	MinTokens int

	// KeywordsAny matches when ANY of the listed strings appears
	// (case-insensitive substring) in the joined user-message
	// content. Empty list = skip this check.
	KeywordsAny []string

	// KeywordsNone matches when NONE of the listed strings appears
	// in the user content. Useful for "default route except for
	// these tricky topics".
	KeywordsNone []string
}

// RuleClassifier evaluates rules in order. The Tokenizer dep is
// optional — when nil, MaxTokens / MinTokens checks are skipped (the
// rule still matches if its other conditions hold, which is the
// degenerate path you'd hit in dev mode without tokenizer wired).
type RuleClassifier struct {
	rules     []Rule
	tokenizer *tokenizer.Selector
}

// NewRuleClassifier validates rules and returns a Classifier. Errors
// surface only misconfiguration: missing names, duplicate names,
// missing RouteTo. Per-request errors are impossible by construction.
func NewRuleClassifier(rules []Rule, tk *tokenizer.Selector) (*RuleClassifier, error) {
	seen := make(map[string]struct{}, len(rules))
	for i, r := range rules {
		if r.Name == "" {
			return nil, fmt.Errorf("route: rule[%d] missing Name", i)
		}
		if r.RouteTo == "" {
			return nil, fmt.Errorf("route: rule %q missing RouteTo", r.Name)
		}
		if _, dup := seen[r.Name]; dup {
			return nil, fmt.Errorf("route: duplicate rule name %q", r.Name)
		}
		seen[r.Name] = struct{}{}
	}
	return &RuleClassifier{rules: rules, tokenizer: tk}, nil
}

// Classify walks the rule list and returns the first match. Empty
// Decision (no rule fired) means "use the request's original model".
//
// Token counting is lazy: only computed if a rule actually examines
// MaxTokens / MinTokens. Many requests are answered by keyword-only
// rules and never pay tokenizer cost.
func (c *RuleClassifier) Classify(req *provider.ChatRequest) Decision {
	if req == nil || len(c.rules) == 0 {
		return Decision{}
	}

	var tokenCount int
	tokenCounted := false

	// Lower-cased user content is computed once on first keyword
	// rule; rules that don't use keywords skip the allocation.
	var userContent string
	contentJoined := false

	for _, r := range c.rules {
		if r.When.MaxTokens > 0 || r.When.MinTokens > 0 {
			if !tokenCounted {
				tokenCount = c.countTokens(req)
				tokenCounted = true
			}
			if r.When.MaxTokens > 0 && tokenCount > r.When.MaxTokens {
				continue
			}
			if r.When.MinTokens > 0 && tokenCount < r.When.MinTokens {
				continue
			}
		}

		if len(r.When.KeywordsAny) > 0 || len(r.When.KeywordsNone) > 0 {
			if !contentJoined {
				userContent = strings.ToLower(joinUserContent(req))
				contentJoined = true
			}
			if len(r.When.KeywordsAny) > 0 && !anyKeywordPresent(userContent, r.When.KeywordsAny) {
				continue
			}
			if len(r.When.KeywordsNone) > 0 && anyKeywordPresent(userContent, r.When.KeywordsNone) {
				continue
			}
		}

		return Decision{Model: r.RouteTo, Rule: r.Name}
	}
	return Decision{}
}

// Rules exposes the configured ruleset for ops/debug surfaces. Order
// is preserved (matches evaluation order).
func (c *RuleClassifier) Rules() []Rule { return c.rules }

// countTokens returns the prompt-token count via the configured
// tokenizer. Returns 0 when no tokenizer is wired — token-based rules
// then never match, which is the correct degenerate-mode behavior.
func (c *RuleClassifier) countTokens(req *provider.ChatRequest) int {
	if c.tokenizer == nil {
		return 0
	}
	return c.tokenizer.For(req.Model).CountMessages(req.Messages)
}

// joinUserContent concatenates all user-role message contents so a
// single substring search covers multi-turn conversations. Skipping
// system / assistant / tool roles avoids matching the gateway's own
// routing keywords if they appeared in a system prompt.
func joinUserContent(req *provider.ChatRequest) string {
	var b strings.Builder
	for _, m := range req.Messages {
		if m.Role != "user" {
			continue
		}
		if b.Len() > 0 {
			b.WriteByte(' ')
		}
		b.WriteString(m.Content)
	}
	return b.String()
}

// anyKeywordPresent reports whether any keyword appears in haystack.
// haystack is expected pre-lowered; keywords are lowered here so a
// single rule's keyword list isn't iterated more than once at runtime.
func anyKeywordPresent(haystack string, keywords []string) bool {
	for _, kw := range keywords {
		if kw == "" {
			continue
		}
		if strings.Contains(haystack, strings.ToLower(kw)) {
			return true
		}
	}
	return false
}

// Compile-time interface guard.
var _ Classifier = (*RuleClassifier)(nil)

// Sentinel returned by NewRuleClassifier callers when constructing
// from raw input fails — exported so cmd/gateway assembly can
// errors.Is against it for boot-time messaging.
var ErrInvalidRule = errors.New("route: invalid rule configuration")
