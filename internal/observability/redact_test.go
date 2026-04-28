package observability

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestHashPreview_Empty(t *testing.T) {
	assert.Equal(t, "", HashPreview(""))
}

func TestHashPreview_Stable(t *testing.T) {
	a := HashPreview("hello world")
	b := HashPreview("hello world")
	assert.Equal(t, a, b)
	assert.True(t, strings.HasPrefix(a, "sha256:"))
	assert.Len(t, a, len("sha256:")+8) // 8 hex chars
}

func TestHashPreview_Differs(t *testing.T) {
	a := HashPreview("hello")
	b := HashPreview("world")
	assert.NotEqual(t, a, b)
}

func TestHashPreview_NoLeak(t *testing.T) {
	// The whole point: a long sensitive string must not appear in the
	// preview. We grep for known tokens in the original.
	preview := HashPreview("supersecret-prompt-canary")
	assert.NotContains(t, preview, "supersecret")
	assert.NotContains(t, preview, "canary")
}

func TestKeyPreview(t *testing.T) {
	assert.Equal(t, "<short>", KeyPreview(""))
	assert.Equal(t, "<short>", KeyPreview("sk-tiny"))
	assert.Equal(t, "sk-abc...", KeyPreview("sk-abcdef-very-long-key-12345"))

	// Boundary: exactly 12 chars → first 6 + "..."
	assert.Equal(t, "abcdef...", KeyPreview("abcdef123456"))
}
