package auth

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
)

// StaticAuthenticator validates keys against an in-memory table, keyed by
// SHA-256(secret). Hashing on load means the raw secret never persists in
// the running process beyond the construction call, narrowing the blast
// radius of e.g. a heap dump.
//
// Safe for concurrent use after construction (the underlying map is
// read-only).
type StaticAuthenticator struct {
	byHash map[string]*Principal
}

// NewStatic builds a StaticAuthenticator from a slice of (id, name, secret)
// entries. Returns an error if:
//   - any entry has an empty id or secret
//   - two entries share the same id
//   - two entries share the same secret (would silently mask one principal)
//
// The returned authenticator is safe for concurrent use.
func NewStatic(entries []StaticEntry) (*StaticAuthenticator, error) {
	if len(entries) == 0 {
		return nil, errors.New("auth: at least one key entry is required")
	}

	byHash := make(map[string]*Principal, len(entries))
	seenID := make(map[string]struct{}, len(entries))

	var errs []error
	for i, e := range entries {
		if e.ID == "" {
			errs = append(errs, fmt.Errorf("auth: entry[%d]: id is required", i))
			continue
		}
		if e.Secret == "" {
			errs = append(errs, fmt.Errorf("auth: entry[%d] %q: secret is required", i, e.ID))
			continue
		}
		if _, dup := seenID[e.ID]; dup {
			errs = append(errs, fmt.Errorf("auth: entry[%d]: duplicate id %q", i, e.ID))
			continue
		}
		seenID[e.ID] = struct{}{}

		hash := hashKey(e.Secret)
		if existing, dup := byHash[hash]; dup {
			errs = append(errs, fmt.Errorf("auth: entry[%d] %q: secret collides with id %q", i, e.ID, existing.ID))
			continue
		}
		byHash[hash] = &Principal{ID: e.ID, Name: e.Name}
	}

	if len(errs) > 0 {
		return nil, errors.Join(errs...)
	}
	return &StaticAuthenticator{byHash: byHash}, nil
}

// StaticEntry is one row of the static key table.
type StaticEntry struct {
	ID     string
	Name   string
	Secret string
}

// Authenticate looks up a Principal by SHA-256 of the supplied key.
// Empty key → ErrMissingCredentials; unknown key → ErrInvalidCredentials.
//
// Constant-time-ish: the only secret-dependent operation is the SHA-256
// hash; the subsequent map lookup is on a 32-byte hex digest which has no
// secret-correlated branch.
func (s *StaticAuthenticator) Authenticate(_ context.Context, key string) (*Principal, error) {
	if key == "" {
		return nil, ErrMissingCredentials
	}
	p, ok := s.byHash[hashKey(key)]
	if !ok {
		return nil, ErrInvalidCredentials
	}
	return p, nil
}

// Size returns the number of registered principals. Useful for startup logs.
func (s *StaticAuthenticator) Size() int { return len(s.byHash) }

// hashKey returns the lowercase hex SHA-256 of secret.
func hashKey(secret string) string {
	sum := sha256.Sum256([]byte(secret))
	return hex.EncodeToString(sum[:])
}
