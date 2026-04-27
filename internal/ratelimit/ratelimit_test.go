package ratelimit

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// fakeLimiter is a scriptable Limiter used by Rule / Multi tests. It
// records the key that was passed in (so we can assert composeKey did
// the right thing) and lets the test pre-program the Decision returned.
type fakeLimiter struct {
	resp   Decision
	err    error
	calls  []string // keys observed, in call order
	failOn string   // when non-empty, return err for keys matching this exact string
}

func (f *fakeLimiter) Allow(_ context.Context, key string, _ int) (Decision, error) {
	f.calls = append(f.calls, key)
	if f.failOn != "" && key == f.failOn {
		return Decision{}, f.err
	}
	return f.resp, nil
}

func TestRule_ComposeKey_Global(t *testing.T) {
	// Empty KeyBy: composeKey = "ratelimit:<name>".
	r := &Rule{Name: "global-rps", Limiter: &fakeLimiter{resp: Decision{Allowed: true, Remaining: 5}}}
	got := r.composeKey(KeyContext{APIKeyID: "k1", Model: "gpt"})
	assert.Equal(t, "ratelimit:global-rps", got)
}

func TestRule_ComposeKey_SingleDim(t *testing.T) {
	r := &Rule{Name: "per-key", KeyBy: []KeyBy{KeyByAPIKey}}
	assert.Equal(t, "ratelimit:per-key:abc", r.composeKey(KeyContext{APIKeyID: "abc"}))
}

func TestRule_ComposeKey_TwoDims(t *testing.T) {
	r := &Rule{Name: "per-key-model", KeyBy: []KeyBy{KeyByAPIKey, KeyByModel}}
	got := r.composeKey(KeyContext{APIKeyID: "k1", Model: "gpt-4o"})
	assert.Equal(t, "ratelimit:per-key-model:k1:gpt-4o", got)
}

func TestRule_ComposeKey_MissingDim(t *testing.T) {
	// Missing dimension renders as empty string between colons; rule name
	// keeps the key unique vs. a different rule's "ratelimit:other:k1:".
	r := &Rule{Name: "per-key-model", KeyBy: []KeyBy{KeyByAPIKey, KeyByModel}}
	got := r.composeKey(KeyContext{APIKeyID: "k1"})
	assert.Equal(t, "ratelimit:per-key-model:k1:", got)
}

func TestRule_AllowSetsRuleName(t *testing.T) {
	f := &fakeLimiter{resp: Decision{Allowed: true, Remaining: 5}}
	r := &Rule{Name: "test-rule", Limiter: f}
	d, err := r.Allow(context.Background(), KeyContext{APIKeyID: "x"}, 1)
	require.NoError(t, err)
	assert.Equal(t, "test-rule", d.Rule, "Rule.Allow must stamp Decision.Rule")
}

func TestMulti_Empty_AllowsButReportsZero(t *testing.T) {
	m := NewMulti()
	d, err := m.Check(context.Background(), KeyContext{}, 1)
	require.NoError(t, err)
	assert.True(t, d.Allowed)
	assert.Zero(t, d.Limit)
}

func TestMulti_AllPass_ReturnsTightest(t *testing.T) {
	loose := &fakeLimiter{resp: Decision{Allowed: true, Limit: 1000, Remaining: 999}}
	tight := &fakeLimiter{resp: Decision{Allowed: true, Limit: 60, Remaining: 3}}

	m := NewMulti(
		&Rule{Name: "global", Limiter: loose},
		&Rule{Name: "per-key", KeyBy: []KeyBy{KeyByAPIKey}, Limiter: tight},
	)
	d, err := m.Check(context.Background(), KeyContext{APIKeyID: "k1"}, 1)
	require.NoError(t, err)
	assert.True(t, d.Allowed)
	// Tightest has Remaining=3; that's what the response should advertise.
	assert.Equal(t, 3, d.Remaining)
	assert.Equal(t, 60, d.Limit)
	assert.Equal(t, "per-key", d.Rule)
}

func TestMulti_FirstDenyWins(t *testing.T) {
	pass := &fakeLimiter{resp: Decision{Allowed: true, Remaining: 5}}
	deny := &fakeLimiter{resp: Decision{Allowed: false, Limit: 60, Remaining: 0, RetryAfter: 30 * time.Second}}
	never := &fakeLimiter{resp: Decision{Allowed: true, Remaining: 999}}

	m := NewMulti(
		&Rule{Name: "global", Limiter: pass},
		&Rule{Name: "per-key", Limiter: deny},
		&Rule{Name: "must-not-run", Limiter: never},
	)
	d, err := m.Check(context.Background(), KeyContext{}, 1)
	require.NoError(t, err)
	assert.False(t, d.Allowed)
	assert.Equal(t, "per-key", d.Rule)
	assert.Equal(t, 30*time.Second, d.RetryAfter)
	assert.Empty(t, never.calls, "rule after first deny must not be evaluated")
}

func TestMulti_BackendErrorPropagates(t *testing.T) {
	flaky := &fakeLimiter{
		resp:   Decision{Allowed: true},
		err:    errors.New("redis: connection refused"),
		failOn: "ratelimit:flaky",
	}
	m := NewMulti(&Rule{Name: "flaky", Limiter: flaky})
	_, err := m.Check(context.Background(), KeyContext{}, 1)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "connection refused")
}

func TestKeyContext_ValueUnknown(t *testing.T) {
	k := KeyContext{APIKeyID: "x", Model: "y"}
	assert.Equal(t, "x", k.value(KeyByAPIKey))
	assert.Equal(t, "y", k.value(KeyByModel))
	assert.Equal(t, "", k.value(KeyBy("ip"))) // future dim, currently unsupported
}
