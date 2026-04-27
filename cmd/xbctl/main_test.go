package main

import (
	"bytes"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestRun_NoArgs_PrintsUsageAndErrors(t *testing.T) {
	var stdout, stderr bytes.Buffer
	err := run(nil, &stdout, &stderr)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "subcommand required")
	assert.Contains(t, stdout.String(), "Subcommands")
}

func TestRun_HelpPrintsUsage(t *testing.T) {
	for _, arg := range []string{"-h", "--help", "help"} {
		t.Run(arg, func(t *testing.T) {
			var stdout, stderr bytes.Buffer
			err := run([]string{arg}, &stdout, &stderr)
			require.NoError(t, err)
			assert.Contains(t, stdout.String(), "Subcommands")
		})
	}
}

func TestRun_VersionFlag(t *testing.T) {
	var stdout, stderr bytes.Buffer
	err := run([]string{"--version"}, &stdout, &stderr)
	require.NoError(t, err)
	assert.Contains(t, stdout.String(), "xbctl")
}

func TestRun_UnknownSubcommand(t *testing.T) {
	var stdout, stderr bytes.Buffer
	err := run([]string{"bogus"}, &stdout, &stderr)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "unknown subcommand")
}

func TestKeygen_RequiresName(t *testing.T) {
	var stdout bytes.Buffer
	err := runKeygen(nil, &stdout)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "name is required")
}

func TestKeyrevoke_RequiresID(t *testing.T) {
	var stdout bytes.Buffer
	err := runKeyrevoke(nil, &stdout)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "id is required")
}

func TestMigrate_RequiresSubaction(t *testing.T) {
	var stdout bytes.Buffer
	err := runMigrate(nil, &stdout)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "up | down | version")
}

func TestMigrate_RejectsUnknownSubaction(t *testing.T) {
	var stdout bytes.Buffer
	err := runMigrate([]string{"-dsn", "postgres://x", "sideways"}, &stdout)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "unknown subaction")
}

func TestGenerateSecret_ShapeAndUniqueness(t *testing.T) {
	s1, err := generateSecret()
	require.NoError(t, err)
	s2, err := generateSecret()
	require.NoError(t, err)

	assert.NotEqual(t, s1, s2, "secrets must be unique across calls")
	assert.True(t, strings.HasPrefix(s1, "sk-"), "want sk- prefix, got %q", s1)
	assert.Equal(t, 46, len(s1),
		"expected 46 chars (3 prefix + 43 base64url-no-pad of 32 bytes), got %d in %q",
		len(s1), s1)
}

func TestShortHash(t *testing.T) {
	assert.Equal(t, "abcdefabcdef", shortHash("abcdefabcdef0123456789"))
	assert.Equal(t, "abc", shortHash("abc"))
}

func TestFormatTime_NilDash(t *testing.T) {
	assert.Equal(t, "-", formatTime(nil))
}
