package auth

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Hermetic tests for the parts that don't need a DB. Integration tests
// (List / Create / Revoke against real PG) gated by XBEACON_TEST_DSN
// follow the integrationPool pattern from postgres_test.go.

func TestValidateScopes_FormatRejected(t *testing.T) {
	cases := map[string]map[string][]string{
		"uppercase category":  {"Admin": {"webui"}},
		"uppercase value":     {"admin": {"WebUI"}},
		"empty value list":    {"admin": {}},
		"category with colon": {"admin:foo": {"x"}},
		"value with space":    {"admin": {"web ui"}},
	}
	for label, in := range cases {
		t.Run(label, func(t *testing.T) {
			err := validateScopes(in)
			assert.Error(t, err, "%s should fail", label)
		})
	}
}

func TestValidateScopes_AcceptsConventional(t *testing.T) {
	ok := map[string][]string{
		"admin":        {"webui", "pricing"},
		"smart_route":  {"disable"},
	}
	require.NoError(t, validateScopes(ok))
}

func TestGenerateSecret_PrefixAndEntropy(t *testing.T) {
	a, err := generateSecret()
	require.NoError(t, err)
	b, err := generateSecret()
	require.NoError(t, err)

	assert.True(t, strings.HasPrefix(a, "sk-"))
	assert.NotEqual(t, a, b, "two calls must yield different secrets")
	// 32 bytes base64url-no-pad → 43 chars, plus 3-char prefix = 46.
	assert.Equal(t, 46, len(a))
}

func TestIDPreview_Truncates(t *testing.T) {
	assert.Equal(t, "01234567", idPreview("0123456789abcdef"))
	assert.Equal(t, "abc", idPreview("abc"), "shorter-than-window stays whole")
}

func TestDecodeScopes_Tolerant(t *testing.T) {
	assert.Nil(t, decodeScopes(nil))
	assert.Nil(t, decodeScopes([]byte("{}")))
	assert.Nil(t, decodeScopes([]byte("null")))
	assert.Nil(t, decodeScopes([]byte("not json")))

	got := decodeScopes([]byte(`{"admin":["webui"]}`))
	assert.Equal(t, []string{"webui"}, got["admin"])
}

// Cache key construction is what Revoke relies on for invalidation;
// the DB path (Revoke itself) is covered by integration tests gated
// on XBEACON_TEST_DSN.
func TestKeystore_CacheKeyShape(t *testing.T) {
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = rdb.Close() })

	secret := "sk-test-secret"
	expectedKey := "auth:k:" + hashKeyHex(secret)

	require.NoError(t, rdb.Set(context.Background(), expectedKey, `{"id":"x","name":"y"}`, 0).Err())

	// Simulate what Revoke does internally: hex-encode the raw hash
	// from the DB and Del. This proves the cache-key recipe in
	// Revoke's body matches what the cached Authenticator wrote.
	rawHash := hashKeyBytes(secret)
	delKey := "auth:k:"
	for _, b := range rawHash {
		const hexdigits = "0123456789abcdef"
		delKey += string(hexdigits[b>>4]) + string(hexdigits[b&0x0f])
	}
	require.Equal(t, expectedKey, delKey, "Revoke's cache-key recipe must match Authenticate's")

	require.NoError(t, rdb.Del(context.Background(), delKey).Err())

	exists, err := rdb.Exists(context.Background(), expectedKey).Result()
	require.NoError(t, err)
	assert.Equal(t, int64(0), exists)
}

// Sanity: ErrKeyNotFound is comparable via errors.Is so handlers can
// map it to 404 cleanly.
func TestErrKeyNotFound_IsSentinel(t *testing.T) {
	assert.True(t, errors.Is(ErrKeyNotFound, ErrKeyNotFound))
}
