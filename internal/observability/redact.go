package observability

import (
	"crypto/sha256"
	"encoding/hex"
)

// HashPreview returns a short, redaction-safe identifier for a piece of
// text — typically a user prompt or assistant response — so it can be
// correlated with client-side logs without leaking content.
//
// Output shape:
//
//	"sha256:abcd1234"  for non-empty inputs (8 hex chars = 32 bits of
//	                   collision resistance, plenty for cross-log
//	                   correlation; not a security primitive)
//	""                 for empty input (no need for a "hash of nothing")
//
// Per Week 8 carry-over decision: error-path logs use this helper instead
// of the raw text. happy-path logs record only token counts (no preview).
func HashPreview(text string) string {
	if text == "" {
		return ""
	}
	sum := sha256.Sum256([]byte(text))
	return "sha256:" + hex.EncodeToString(sum[:4])
}

// KeyPreview redacts a bearer token / API key down to its public-safe
// prefix. Returns the first 6 characters when the input is at least 12
// long; shorter inputs are reduced to "<short>" so the actual length
// isn't even hinted at. Used by audit logs that want to record "which
// key" without enabling reconstruction.
//
// IMPORTANT: prefer logging the principal.id when available — this
// helper is only for paths that have a raw key but no resolved
// principal yet (e.g. early auth-failure logging).
func KeyPreview(key string) string {
	if len(key) < 12 {
		return "<short>"
	}
	return key[:6] + "..."
}
