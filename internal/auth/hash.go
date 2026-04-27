package auth

import (
	"crypto/sha256"
	"encoding/hex"
)

// hashKeyBytes returns the raw 32-byte SHA-256 of secret. Used by the
// Postgres backend (BYTEA column) and by the cache decorator (binary
// Redis key).
func hashKeyBytes(secret string) []byte {
	sum := sha256.Sum256([]byte(secret))
	return sum[:]
}

// hashKeyHex returns the lowercase hex SHA-256 of secret. Used by the
// Static backend (Go map key — slices aren't comparable, so we encode).
func hashKeyHex(secret string) string {
	sum := sha256.Sum256([]byte(secret))
	return hex.EncodeToString(sum[:])
}
