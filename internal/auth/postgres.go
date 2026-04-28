package auth

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"

	"github.com/An-idd/x-beacon/internal/storage"
)

// PostgresAuthenticator looks up keys in the api_keys table:
//
//	SELECT id, name FROM api_keys
//	WHERE key_hash = $1 AND revoked_at IS NULL
//
// On a successful lookup, last_used_at is bumped to now() in the same
// statement (RETURNING + UPDATE) so audit data stays accurate without a
// second roundtrip. The cache decorator (Step 4.4) will absorb most of
// this load, so the per-request DB write is bounded by cache misses.
//
// Safe for concurrent use; the *storage.Pool itself is concurrency-safe.
type PostgresAuthenticator struct {
	pool *storage.Pool
}

// NewPostgres builds an Authenticator backed by the given pool. The pool
// is not pinged here — main pings it at startup separately and decides
// whether to fall back to "no auth" dev mode.
func NewPostgres(pool *storage.Pool) *PostgresAuthenticator {
	return &PostgresAuthenticator{pool: pool}
}

// Authenticate hashes key and looks up the active principal. Returns
// ErrMissingCredentials for an empty key, ErrInvalidCredentials when
// no active row matches, or a wrapped error for unexpected DB failures.
func (a *PostgresAuthenticator) Authenticate(ctx context.Context, key string) (*Principal, error) {
	if key == "" {
		return nil, ErrMissingCredentials
	}
	if a == nil || a.pool == nil {
		return nil, errors.New("auth: postgres authenticator has no pool")
	}

	// Single-statement lookup + last_used_at refresh. RETURNING gives us
	// id+name without a second SELECT. The partial index
	// idx_api_keys_active_hash makes this an index-only scan over
	// non-revoked rows.
	const q = `
		UPDATE api_keys
		   SET last_used_at = now()
		 WHERE key_hash = $1
		   AND revoked_at IS NULL
		RETURNING id, name, scopes`

	hash := hashKeyBytes(key)

	var p Principal
	var scopesRaw []byte
	err := a.pool.QueryRow(ctx, q, hash).Scan(&p.ID, &p.Name, &scopesRaw)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		return nil, ErrInvalidCredentials
	case err != nil:
		// Wrap so callers can errors.Is for ErrInvalidCredentials without
		// false-matching DB errors. Middleware logs at warn and returns 500.
		return nil, fmt.Errorf("auth: query api_keys: %w", err)
	}
	// scopes column defaults to '{}'; tolerate empty / null so old rows
	// without the feature don't cause auth failures.
	if len(scopesRaw) > 0 && string(scopesRaw) != "{}" && string(scopesRaw) != "null" {
		if err := json.Unmarshal(scopesRaw, &p.Scopes); err != nil {
			return nil, fmt.Errorf("auth: decode scopes for %q: %w", p.ID, err)
		}
	}
	return &p, nil
}
