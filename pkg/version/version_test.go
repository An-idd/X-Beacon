package version

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestUserAgent_Shape(t *testing.T) {
	ua := UserAgent()
	assert.True(t, strings.HasPrefix(ua, "x-beacon/"), "got %q", ua)
}

func TestBanner_ContainsAllFields(t *testing.T) {
	b := Banner()
	assert.Contains(t, b, Version)
	assert.Contains(t, b, Commit)
	assert.Contains(t, b, BuildTime)
}

func TestVersionDefaults(t *testing.T) {
	// Without ldflags injection, defaults must exist so callers don't see
	// an empty UA / banner.
	assert.NotEmpty(t, Version)
	assert.NotEmpty(t, Commit)
	assert.NotEmpty(t, BuildTime)
}
