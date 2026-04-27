package envexpand

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestExpand_SimpleVar(t *testing.T) {
	t.Setenv("X_TEST_FOO", "hello")
	assert.Equal(t, "hello", Expand("${X_TEST_FOO}"))
	assert.Equal(t, "hello world", Expand("${X_TEST_FOO} world"))
}

func TestExpand_UnsetVar_EmptyResult(t *testing.T) {
	assert.Equal(t, "", Expand("${X_TEST_NOT_SET}"))
	assert.Equal(t, "prefix-", Expand("prefix-${X_TEST_NOT_SET}"))
}

func TestExpand_Default_AppliedWhenUnset(t *testing.T) {
	assert.Equal(t, "fallback", Expand("${X_TEST_NOT_SET:-fallback}"))
}

func TestExpand_Default_AppliedWhenEmpty(t *testing.T) {
	t.Setenv("X_TEST_EMPTY", "")
	assert.Equal(t, "fallback", Expand("${X_TEST_EMPTY:-fallback}"))
}

func TestExpand_Default_IgnoredWhenSet(t *testing.T) {
	t.Setenv("X_TEST_SET", "real")
	assert.Equal(t, "real", Expand("${X_TEST_SET:-fallback}"))
}

func TestExpand_MultipleInOneString(t *testing.T) {
	t.Setenv("X_A", "alpha")
	t.Setenv("X_B", "beta")
	assert.Equal(t, "alpha/beta", Expand("${X_A}/${X_B}"))
}

func TestExpand_NoPlaceholders_Passthrough(t *testing.T) {
	assert.Equal(t, "plain string with $ sign", Expand("plain string with $ sign"))
}

func TestExpand_BareDollarNotExpanded(t *testing.T) {
	t.Setenv("X_TEST_BARE", "expanded")
	assert.Equal(t, "$X_TEST_BARE", Expand("$X_TEST_BARE"))
}

func TestExpand_EmptyDefault(t *testing.T) {
	assert.Equal(t, "", Expand("${X_TEST_NOT_SET:-}"))
}

func TestExpand_DefaultWithSpecialChars(t *testing.T) {
	assert.Equal(t, "http://host:8080", Expand("${X_TEST_NOT_SET:-http://host:8080}"))
}
