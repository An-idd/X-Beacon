// Package auth holds the gateway's authentication primitives.
//
// Week 3 (this step) ships v1: a static, in-memory key table loaded from
// configs/auth.yaml. Week 4 swaps in a DB-backed implementation; the
// Authenticator interface is the only contract the rest of the gateway
// depends on, so that swap is local.
package auth

import (
	"context"
	"errors"
)

// Sentinel errors. Callers (notably the auth middleware) classify failures
// using errors.Is so the storage backend can return wrapped variants.
var (
	// ErrMissingCredentials is returned when no Authorization header /
	// bearer token was supplied.
	ErrMissingCredentials = errors.New("auth: missing credentials")

	// ErrInvalidCredentials is returned when a key is supplied but does
	// not match any registered principal. Distinct from ErrMissingCredentials
	// so the middleware can pick a 401 vs 401 with a different message
	// (and so audit logs differentiate "no key" from "wrong key").
	ErrInvalidCredentials = errors.New("auth: invalid credentials")
)

// Principal identifies the API consumer behind a request. Fields beyond
// ID/Name (scopes, rate-limit class) come in Week 4 once DB-backed auth
// gives us a place to store them.
//
// IMPORTANT: Principal must NEVER carry the raw key secret. The middleware
// stores Principal in request context; downstream handlers and log lines
// may read any field on it.
type Principal struct {
	// ID is the stable, non-secret identifier configured for the key
	// (e.g. "dev-key-1"). Safe to log.
	ID string

	// Name is a human-readable label (e.g. "Local development"). Safe to log.
	Name string

	// Scopes carries the JSONB `scopes` column (Week 4 schema reserved
	// it for forward use). Convention: top-level keys are categories
	// ("admin", "rate"), values are arrays of allowed verbs/resources.
	// Example: {"admin": ["pricing"]} grants /admin/pricing access.
	// nil for keys created before the scope feature; HasScope handles
	// nil receivers safely.
	Scopes map[string][]string
}

// HasScope reports whether the principal carries `value` (or the
// wildcard "*") under the given category. nil-safe so handlers can
// `auth.PrincipalFrom(ctx).HasScope(...)` without prior nil check.
func (p *Principal) HasScope(category, value string) bool {
	if p == nil {
		return false
	}
	for _, v := range p.Scopes[category] {
		if v == "*" || v == value {
			return true
		}
	}
	return false
}

// Authenticator validates a raw bearer token and returns the Principal it
// represents. Implementations must:
//
//   - return ErrMissingCredentials when key is empty
//   - return ErrInvalidCredentials when key does not match any principal
//   - run in constant time wrt the secret material (defense against timing
//     side channels) — the static implementation hashes once and compares
//     fixed-size digests, which satisfies this without explicit
//     subtle.ConstantTimeCompare per lookup
type Authenticator interface {
	Authenticate(ctx context.Context, key string) (*Principal, error)
}

// principalKey is the unexported context key used by the middleware to
// thread the authenticated Principal down to handlers.
type principalKey struct{}

// WithPrincipal returns a copy of ctx carrying p. Used by the auth
// middleware after a successful Authenticate; tests may also use it to
// fabricate authenticated requests.
func WithPrincipal(ctx context.Context, p *Principal) context.Context {
	return context.WithValue(ctx, principalKey{}, p)
}

// PrincipalFrom returns the Principal stored on ctx by WithPrincipal,
// or nil if no authentication has occurred.
func PrincipalFrom(ctx context.Context) *Principal {
	if v, ok := ctx.Value(principalKey{}).(*Principal); ok {
		return v
	}
	return nil
}
