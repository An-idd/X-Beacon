package auth

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/redis/go-redis/v9"
	"go.uber.org/zap"

	"github.com/An-idd/x-beacon/internal/storage"
)

// KeyRecord is the projected public shape of an api_keys row. The
// secret is NEVER on this struct — see Create for the one-shot return
// path.
//
// HashHexShort is the first 12 chars of the hex SHA-256, exposed only
// for ops debugging (correlating to Redis cache keys via SCAN). The
// full hash never crosses an HTTP boundary; the WebUI shows
// IDPreview only.
type KeyRecord struct {
	ID           string
	IDPreview    string
	Name         string
	HashHexShort string
	Scopes       map[string][]string
	CreatedAt    time.Time
	LastUsedAt   *time.Time
	RevokedAt    *time.Time
}

// ListOpts shapes the List query. NamePrefix is a case-sensitive prefix
// match (DB-side `LIKE prefix || '%'`); empty string disables filtering.
// IncludeRevoked defaults false — admin UIs typically want active keys.
type ListOpts struct {
	NamePrefix     string
	IncludeRevoked bool
	Limit          int // clamped to [1, 200]
	Offset         int // >= 0
}

// Keystore is the read/write façade for api_keys, shared by xbctl and
// /admin/keys. It owns:
//
//   - the schema-level DDL contract (column names, JSONB shape)
//   - the secret-generation policy (length, prefix, encoding)
//   - the Redis cache invalidation on revoke (so revoked keys stop
//     authenticating immediately, not after the cache TTL window)
//
// Safe for concurrent use; *storage.Pool and redis.UniversalClient are
// both concurrency-safe.
type Keystore struct {
	pool   *storage.Pool
	cache  redis.UniversalClient // optional; nil disables invalidation
	logger *zap.Logger
}

// NewKeystore wires the storage and (optional) cache. Cache may be nil —
// in dev mode without Redis the admin endpoints still work; revoke
// becomes "DB only", which is safe (cache absent → no stale entries).
func NewKeystore(pool *storage.Pool, cache redis.UniversalClient, logger *zap.Logger) *Keystore {
	if logger == nil {
		logger = zap.NewNop()
	}
	return &Keystore{pool: pool, cache: cache, logger: logger}
}

// secretPrefix mirrors OpenAI's "sk-" convention so existing redaction
// tooling in client codebases works unchanged.
const secretPrefix = "sk-"

// secretBytes is the raw entropy. 32 → base64url-no-pad → 43 chars,
// total 46 with the prefix. Comparable to OpenAI / Anthropic.
const secretBytes = 32

// scopePattern enforces `^[a-z][a-z_]*:[a-z][a-z_]*$` — lowercase
// underscored identifiers on each side. Stricter than what xbctl
// accepts so HTTP-side input doesn't poison the table with
// `Admin:WebUI` style casing inconsistencies. Documented contract
// in docs/runbook.md §3.5.
var scopePattern = regexp.MustCompile(`^[a-z][a-z_]*:[a-z][a-z_]*$`)

// ErrKeyNotFound is returned when revoke / get target an absent ID.
var ErrKeyNotFound = errors.New("auth: key not found")

// List returns paginated rows + total count. Total is computed in the
// same SELECT (`COUNT(*) OVER ()`) so a small admin UI doesn't need a
// separate count call. Acceptable while the table is < 100k rows; if
// it grows past that, switch to cursor pagination.
func (k *Keystore) List(ctx context.Context, opts ListOpts) ([]KeyRecord, int, error) {
	if k == nil || k.pool == nil {
		return nil, 0, errors.New("auth: keystore has no pool")
	}
	if opts.Limit <= 0 || opts.Limit > 200 {
		opts.Limit = 50
	}
	if opts.Offset < 0 {
		opts.Offset = 0
	}

	// Argument-positional WHERE assembly: keep it boring rather than
	// chase a fancy query builder.
	where := "WHERE TRUE"
	args := []any{}
	if !opts.IncludeRevoked {
		where += " AND revoked_at IS NULL"
	}
	if opts.NamePrefix != "" {
		args = append(args, opts.NamePrefix)
		where += fmt.Sprintf(" AND name LIKE $%d || '%%'", len(args))
	}
	args = append(args, opts.Limit, opts.Offset)
	q := fmt.Sprintf(`
		SELECT id, name, key_hash, scopes, created_at, last_used_at, revoked_at,
		       COUNT(*) OVER () AS total
		  FROM api_keys
		  %s
		  ORDER BY created_at DESC
		  LIMIT $%d OFFSET $%d`, where, len(args)-1, len(args))

	rows, err := k.pool.Query(ctx, q, args...)
	if err != nil {
		return nil, 0, fmt.Errorf("auth: list api_keys: %w", err)
	}
	defer rows.Close()

	var (
		out   []KeyRecord
		total int
	)
	for rows.Next() {
		var (
			r          KeyRecord
			hashRaw    []byte
			scopesRaw  []byte
		)
		if err := rows.Scan(&r.ID, &r.Name, &hashRaw, &scopesRaw,
			&r.CreatedAt, &r.LastUsedAt, &r.RevokedAt, &total); err != nil {
			return nil, 0, fmt.Errorf("auth: scan api_keys row: %w", err)
		}
		r.IDPreview = idPreview(r.ID)
		r.HashHexShort = shortHashHex(hashRaw)
		r.Scopes = decodeScopes(scopesRaw)
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		return nil, 0, fmt.Errorf("auth: iterate api_keys: %w", err)
	}
	return out, total, nil
}

// Create inserts a new key. Returns the projection PLUS the freshly
// generated plaintext secret. The secret is the only path by which the
// secret ever leaves this process — log nothing, persist nothing.
//
// Caller responsibilities:
//   - Surface the secret to the human exactly once
//   - Never write it to logs / metrics / audit table
//   - Respect the response shape's "shown only once" warning
func (k *Keystore) Create(ctx context.Context, name string, scopes map[string][]string) (KeyRecord, string, error) {
	if k == nil || k.pool == nil {
		return KeyRecord{}, "", errors.New("auth: keystore has no pool")
	}
	if name == "" {
		return KeyRecord{}, "", errors.New("auth: name is required")
	}
	if len(name) > 64 {
		return KeyRecord{}, "", errors.New("auth: name must be <= 64 chars")
	}
	if err := validateScopes(scopes); err != nil {
		return KeyRecord{}, "", err
	}

	id := uuid.NewString()
	secret, err := generateSecret()
	if err != nil {
		return KeyRecord{}, "", fmt.Errorf("auth: generate secret: %w", err)
	}
	hash := sha256.Sum256([]byte(secret))

	scopesJSON, err := json.Marshal(scopes)
	if err != nil {
		return KeyRecord{}, "", fmt.Errorf("auth: encode scopes: %w", err)
	}
	if string(scopesJSON) == "null" {
		scopesJSON = []byte(`{}`)
	}

	const q = `
		INSERT INTO api_keys (id, key_hash, name, scopes)
		     VALUES ($1, $2, $3, $4)
		  RETURNING id, name, key_hash, scopes, created_at, last_used_at, revoked_at`
	var (
		rec       KeyRecord
		hashRaw   []byte
		scopesRaw []byte
	)
	if err := k.pool.QueryRow(ctx, q, id, hash[:], name, scopesJSON).Scan(
		&rec.ID, &rec.Name, &hashRaw, &scopesRaw,
		&rec.CreatedAt, &rec.LastUsedAt, &rec.RevokedAt); err != nil {
		return KeyRecord{}, "", fmt.Errorf("auth: insert api_key: %w", err)
	}
	rec.IDPreview = idPreview(rec.ID)
	rec.HashHexShort = shortHashHex(hashRaw)
	rec.Scopes = decodeScopes(scopesRaw)
	return rec, secret, nil
}

// Revoke flips revoked_at to now() if the key is still active. On a
// successful flip, the auth cache entry (auth:k:<hex>) is best-effort
// deleted so the next request with this key takes the DB path and gets
// 401. Idempotent: returns the existing row when already revoked.
func (k *Keystore) Revoke(ctx context.Context, id string) (KeyRecord, error) {
	if k == nil || k.pool == nil {
		return KeyRecord{}, errors.New("auth: keystore has no pool")
	}
	if id == "" {
		return KeyRecord{}, errors.New("auth: id is required")
	}

	// Conditional update + RETURNING + a CTE so we get the post-update
	// row in a single roundtrip whether or not the flip happened. The
	// COALESCE keeps revoked_at stable for already-revoked keys.
	const q = `
		WITH upsert AS (
			UPDATE api_keys
			   SET revoked_at = COALESCE(revoked_at, now())
			 WHERE id = $1
			RETURNING id, name, key_hash, scopes, created_at, last_used_at, revoked_at
		)
		SELECT * FROM upsert`

	var (
		rec       KeyRecord
		hashRaw   []byte
		scopesRaw []byte
	)
	err := k.pool.QueryRow(ctx, q, id).Scan(
		&rec.ID, &rec.Name, &hashRaw, &scopesRaw,
		&rec.CreatedAt, &rec.LastUsedAt, &rec.RevokedAt)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		return KeyRecord{}, ErrKeyNotFound
	case err != nil:
		return KeyRecord{}, fmt.Errorf("auth: revoke api_key: %w", err)
	}
	rec.IDPreview = idPreview(rec.ID)
	rec.HashHexShort = shortHashHex(hashRaw)
	rec.Scopes = decodeScopes(scopesRaw)

	// Best-effort cache invalidation. The cache is keyed by hex(hash),
	// so we have what we need from hashRaw without ever seeing the
	// secret. A failure here is logged but not surfaced — the operator
	// got a successful revoke either way; the cache will expire on its
	// own posTTL window.
	if k.cache != nil {
		cacheKey := "auth:k:" + hex.EncodeToString(hashRaw)
		if err := k.cache.Del(ctx, cacheKey).Err(); err != nil {
			k.logger.Warn("auth cache invalidation failed; revoke still effective after posTTL",
				zap.String("id", id), zap.Error(err))
		}
	}
	return rec, nil
}

// generateSecret emits an OpenAI-style "sk-<43-char base64url>" token.
// 32 bytes of entropy → 256-bit collision resistance.
func generateSecret() (string, error) {
	buf := make([]byte, secretBytes)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	return secretPrefix + base64.RawURLEncoding.EncodeToString(buf), nil
}

// validateScopes enforces the format contract documented in
// docs/runbook.md §3.5: lowercase `category:value` pairs, no empty
// categories, no empty value lists.
func validateScopes(scopes map[string][]string) error {
	for cat, vals := range scopes {
		if len(vals) == 0 {
			return fmt.Errorf("auth: scope category %q has no values", cat)
		}
		for _, v := range vals {
			tuple := cat + ":" + v
			if !scopePattern.MatchString(tuple) {
				return fmt.Errorf("auth: scope %q invalid (must match %s)", tuple, scopePattern)
			}
		}
	}
	return nil
}

// decodeScopes tolerates the "{}" / "null" / nil cases the same way
// PostgresAuthenticator does — old rows pre-scope feature must not
// cause failures.
func decodeScopes(raw []byte) map[string][]string {
	if len(raw) == 0 || string(raw) == "{}" || string(raw) == "null" {
		return nil
	}
	var s map[string][]string
	if err := json.Unmarshal(raw, &s); err != nil {
		return nil
	}
	return s
}

// idPreview returns the first 8 chars of an ID. Used for log lines and
// for the WebUI's "compact" column. UUIDs are 36 chars; 8 retains
// enough entropy to disambiguate within a single screenful.
func idPreview(id string) string {
	const w = 8
	if len(id) <= w {
		return id
	}
	return id[:w]
}

// shortHashHex returns the first 12 hex chars of the SHA-256. Same as
// xbctl keylist's column so ops can correlate the two surfaces.
func shortHashHex(raw []byte) string {
	const w = 12
	full := hex.EncodeToString(raw)
	if len(full) <= w {
		return full
	}
	return full[:w]
}
