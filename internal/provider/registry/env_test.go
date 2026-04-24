package registry

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestExpandEnv_SimpleVar(t *testing.T) {
	t.Setenv("X_TEST_FOO", "hello")
	assert.Equal(t, "hello", expandEnv("${X_TEST_FOO}"))
	assert.Equal(t, "hello world", expandEnv("${X_TEST_FOO} world"))
}

func TestExpandEnv_UnsetVar_EmptyResult(t *testing.T) {
	assert.Equal(t, "", expandEnv("${X_TEST_NOT_SET}"))
	assert.Equal(t, "prefix-", expandEnv("prefix-${X_TEST_NOT_SET}"))
}

func TestExpandEnv_Default_AppliedWhenUnset(t *testing.T) {
	assert.Equal(t, "fallback", expandEnv("${X_TEST_NOT_SET:-fallback}"))
}

func TestExpandEnv_Default_AppliedWhenEmpty(t *testing.T) {
	t.Setenv("X_TEST_EMPTY", "")
	assert.Equal(t, "fallback", expandEnv("${X_TEST_EMPTY:-fallback}"))
}

func TestExpandEnv_Default_IgnoredWhenSet(t *testing.T) {
	t.Setenv("X_TEST_SET", "real")
	assert.Equal(t, "real", expandEnv("${X_TEST_SET:-fallback}"))
}

func TestExpandEnv_MultipleInOneString(t *testing.T) {
	t.Setenv("X_A", "alpha")
	t.Setenv("X_B", "beta")
	assert.Equal(t, "alpha/beta", expandEnv("${X_A}/${X_B}"))
}

func TestExpandEnv_NoPlaceholders_Passthrough(t *testing.T) {
	assert.Equal(t, "plain string with $ sign", expandEnv("plain string with $ sign"))
}

func TestExpandEnv_BareDollarNotExpanded(t *testing.T) {
	// Unbraced $VAR is not supported — must stay literal.
	t.Setenv("X_TEST_BARE", "expanded")
	assert.Equal(t, "$X_TEST_BARE", expandEnv("$X_TEST_BARE"))
}

func TestExpandEnv_EmptyDefault(t *testing.T) {
	// ${VAR:-} with unset VAR yields the empty fallback.
	assert.Equal(t, "", expandEnv("${X_TEST_NOT_SET:-}"))
}

func TestExpandEnv_DefaultWithSpecialChars(t *testing.T) {
	assert.Equal(t, "http://host:8080", expandEnv("${X_TEST_NOT_SET:-http://host:8080}"))
}
