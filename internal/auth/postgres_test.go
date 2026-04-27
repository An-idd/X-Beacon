package auth

import (
	"context"
	"encoding/hex"
	"errors"
	"os"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/An-idd/x-beacon/internal/storage"
)

func TestPostgres_NilGuards(t *testing.T) {
	var a *PostgresAuthenticator
	_, err := a.Authenticate(context.Background(), "x")
	require.Error(t, err)

	a2 := NewPostgres(nil)
	_, err = a2.Authenticate(context.Background(), "x")
	require.Error(t, err)
}

func TestPostgres_EmptyKey(t *testing.T) {
	// Empty-key shortcut runs before pool dereference, so we don't need a
	// real pool here.
	a := NewPostgres(&storage.Pool{})
	_, err := a.Authenticate(context.Background(), "")
	require.Error(t, err)
	assert.True(t, errors.Is(err, ErrMissingCredentials))
}

// integrationDSN is the env-gated handle to a real Postgres for the rest
// of these tests. Set XBEACON_TEST_DSN to enable.
func integrationDSN(t *testing.T) string {
	t.Helper()
	dsn := os.Getenv("XBEACON_TEST_DSN")
	if dsn == "" {
		t.Skip("set XBEACON_TEST_DSN to run integration tests")
	}
	return dsn
}

// integrationPool migrates a clean schema and returns a pool ready for
// auth tests. Each test gets a fresh DB via Down/Up so they don't
// interfere with each other.
func integrationPool(t *testing.T) *storage.Pool {
	t.Helper()
	dsn := integrationDSN(t)

	require.NoError(t, storage.MigrateDown(dsn))
	require.NoError(t, storage.MigrateUp(dsn))

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	pool, err := storage.NewPool(ctx, storage.Config{DSN: dsn, MaxConns: 4})
	require.NoError(t, err)
	t.Cleanup(pool.Close)
	return pool
}

// insertKey writes a row directly so tests don't depend on cmd/xbctl
// (built later). Returns the raw secret so the test can authenticate with it.
func insertKey(t *testing.T, pool *storage.Pool, id, name, secret string, revoked bool) {
	t.Helper()
	hash := hashKeyBytes(secret)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	q := `INSERT INTO api_keys (id, key_hash, name) VALUES ($1, $2, $3)`
	_, err := pool.Exec(ctx, q, id, hash, name)
	require.NoError(t, err, "insert key %q (hash=%s)", id, hex.EncodeToString(hash))

	if revoked {
		_, err = pool.Exec(ctx, `UPDATE api_keys SET revoked_at = now() WHERE id = $1`, id)
		require.NoError(t, err)
	}
}

func TestPostgres_AuthenticateValidIntegration(t *testing.T) {
	pool := integrationPool(t)
	insertKey(t, pool, "k1", "Test User", "sk-secret-one", false)

	a := NewPostgres(pool)
	p, err := a.Authenticate(context.Background(), "sk-secret-one")
	require.NoError(t, err)
	assert.Equal(t, "k1", p.ID)
	assert.Equal(t, "Test User", p.Name)

	// last_used_at must be set after a successful auth.
	var hasUsed bool
	err = pool.QueryRow(context.Background(),
		`SELECT last_used_at IS NOT NULL FROM api_keys WHERE id = $1`, "k1",
	).Scan(&hasUsed)
	require.NoError(t, err)
	assert.True(t, hasUsed, "last_used_at should be bumped on auth")
}

func TestPostgres_AuthenticateUnknownKeyIntegration(t *testing.T) {
	pool := integrationPool(t)
	a := NewPostgres(pool)

	_, err := a.Authenticate(context.Background(), "sk-not-registered")
	require.Error(t, err)
	assert.True(t, errors.Is(err, ErrInvalidCredentials))
}

func TestPostgres_AuthenticateRevokedKeyIntegration(t *testing.T) {
	pool := integrationPool(t)
	insertKey(t, pool, "k1", "Test", "sk-revoked", true)

	a := NewPostgres(pool)
	_, err := a.Authenticate(context.Background(), "sk-revoked")
	require.Error(t, err)
	assert.True(t, errors.Is(err, ErrInvalidCredentials),
		"revoked key must be indistinguishable from unknown to the caller")
}

func TestPostgres_PartialIndexUsedIntegration(t *testing.T) {
	// Sanity: confirm the active-key partial index is present so future
	// migrations don't silently drop it.
	pool := integrationPool(t)

	var idx string
	err := pool.QueryRow(context.Background(),
		`SELECT indexname FROM pg_indexes WHERE tablename = 'api_keys' AND indexname = 'idx_api_keys_active_hash'`,
	).Scan(&idx)
	require.NoError(t, err)
	assert.Equal(t, "idx_api_keys_active_hash", idx)
}
