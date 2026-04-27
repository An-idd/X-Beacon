// Package ratelimit defines the rate-limit primitives used by the gateway:
//
//   - Limiter: the bucket abstraction (memory token bucket, Redis window).
//   - Rule: one configured limit + KeyBy composition + name.
//   - Multi: an aggregate that runs every rule and rejects on first deny.
//   - KeyContext: the per-request bag of dimensions rules pluck from.
//
// Wire-up: Rule.Allow → Limiter.Allow → bucket math. Middleware sees Multi
// and calls Multi.Check once per request.
package ratelimit

import (
	"context"
	"errors"
	"strings"
	"time"
)

// Decision is the outcome of a single rule (or the tightest after Multi).
// Fields populate the response headers and 429 body when Allowed is false.
type Decision struct {
	Allowed    bool
	Limit      int           // X-RateLimit-Limit
	Remaining  int           // X-RateLimit-Remaining (after the current request)
	Reset      time.Time     // X-RateLimit-Reset (UTC; when the bucket recovers)
	RetryAfter time.Duration // Retry-After header; 0 when Allowed
	Rule       string        // name of the rule that produced this decision
}

// Limiter is one bucket implementation. Concrete types: MemoryBucket
// (Step 5.2), RedisWindow (Step 5.3). Implementations must be safe for
// concurrent use.
type Limiter interface {
	// Allow checks (and atomically decrements when Allowed) the limit for
	// the composed key. cost is the request's weight; Week 5 always passes
	// 1, future steps may pass token estimates.
	//
	// Backend errors (Redis outage, etc.) propagate as `err`. Multi
	// surfaces them so the middleware can decide fail-open vs fail-closed.
	Allow(ctx context.Context, key string, cost int) (Decision, error)
}

// KeyBy enumerates the dimensions a rule can compose its key from. Empty
// `KeyBy` slice on a rule means the rule is global (one bucket for all
// requests).
type KeyBy string

const (
	KeyByAPIKey KeyBy = "api_key"
	KeyByModel  KeyBy = "model"
)

// KeyContext bundles per-request dimensions. Middleware fills it once,
// rules pluck what they need. Adding a new dimension is a Step here +
// a Step in middleware; the Limiter layer is unaffected.
type KeyContext struct {
	APIKeyID string // auth.Principal.ID; empty when unauthenticated
	Model    string // request body's model field; empty for non-chat endpoints
}

// value returns the string slice element for a KeyBy. Unknown dims and
// missing values both return "" — the composed key still hashes uniquely
// because the rule name is included as a prefix.
func (k KeyContext) value(dim KeyBy) string {
	switch dim {
	case KeyByAPIKey:
		return k.APIKeyID
	case KeyByModel:
		return k.Model
	}
	return ""
}

// Rule wraps a Limiter with the dimensions it keys on and a stable name.
// composeKey produces the namespaced key passed to the Limiter so two
// rules using the same Redis instance can't collide on the same KV.
type Rule struct {
	Name    string
	KeyBy   []KeyBy
	Limiter Limiter
}

// Allow composes the key from kctx and delegates to the Limiter. The
// returned Decision has Rule set to r.Name so callers (Multi, middleware)
// know which rule fired.
func (r *Rule) Allow(ctx context.Context, kctx KeyContext, cost int) (Decision, error) {
	key := r.composeKey(kctx)
	d, err := r.Limiter.Allow(ctx, key, cost)
	d.Rule = r.Name
	return d, err
}

// composeKey joins the namespace, rule name, and each KeyBy dimension
// with ':'. Empty dimensions render as the empty string between colons,
// preserving uniqueness (e.g. global rule → "ratelimit:rule:").
func (r *Rule) composeKey(kctx KeyContext) string {
	var b strings.Builder
	b.Grow(64)
	b.WriteString("ratelimit:")
	b.WriteString(r.Name)
	for _, dim := range r.KeyBy {
		b.WriteByte(':')
		b.WriteString(kctx.value(dim))
	}
	return b.String()
}

// ErrEmptyMulti indicates a Multi was constructed with no rules; Check
// returns this rather than silently allowing every request.
var ErrEmptyMulti = errors.New("ratelimit: Multi has no rules")

// Multi runs each rule in order and applies "first deny wins". When all
// rules permit, Multi returns the Decision from the rule whose Remaining
// is smallest — that's the bottleneck the client is closest to, and
// surfacing it via X-RateLimit-Remaining gives the most useful signal.
type Multi struct {
	rules []*Rule
}

// NewMulti constructs an aggregator from a slice of rules. Order matters
// only for deterministic deny attribution; correctness is unchanged.
func NewMulti(rules ...*Rule) *Multi {
	return &Multi{rules: rules}
}

// Len reports the number of attached rules.
func (m *Multi) Len() int { return len(m.rules) }

// Check evaluates every rule. Returns immediately on the first deny.
// If no rules are configured, returns Allowed=true with empty Decision —
// callers gating on a Multi instance can simply skip the call when
// Multi.Len() == 0.
func (m *Multi) Check(ctx context.Context, kctx KeyContext, cost int) (Decision, error) {
	if len(m.rules) == 0 {
		return Decision{Allowed: true}, nil
	}

	tightest := Decision{Allowed: true, Remaining: -1}
	for _, r := range m.rules {
		d, err := r.Allow(ctx, kctx, cost)
		if err != nil {
			return Decision{}, err
		}
		if !d.Allowed {
			return d, nil
		}
		// Track the rule with the smallest Remaining so the response
		// headers reflect the strictest active limit. -1 sentinel means
		// "no rule observed yet".
		if tightest.Remaining < 0 || d.Remaining < tightest.Remaining {
			tightest = d
		}
	}
	return tightest, nil
}
