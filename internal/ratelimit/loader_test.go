package ratelimit

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestBuild_EmptyReturnsNil(t *testing.T) {
	rules, err := Build(nil, nil)
	require.NoError(t, err)
	assert.Nil(t, rules)
}

func TestBuild_MemoryBucket(t *testing.T) {
	rules, err := Build([]RuleConfig{
		{
			Name:      "global-rps",
			Algorithm: "memory_bucket",
			Rate:      "100/s",
			Burst:     200,
		},
	}, nil)
	require.NoError(t, err)
	require.Len(t, rules, 1)
	assert.Equal(t, "global-rps", rules[0].Name)
	assert.Empty(t, rules[0].KeyBy, "no key_by → global")
}

func TestBuild_MemoryBucket_DefaultBurst(t *testing.T) {
	rules, err := Build([]RuleConfig{
		{Name: "rpm", Algorithm: "memory_bucket", Rate: "60/m"},
	}, nil)
	require.NoError(t, err)
	require.Len(t, rules, 1)

	// 60/m = 1 token/s; default burst falls back to int(rate) = 1.
	mb, ok := rules[0].Limiter.(*MemoryBucket)
	require.True(t, ok)
	assert.Equal(t, 1, mb.burst)
}

func TestBuild_RedisWindow(t *testing.T) {
	client, _ := newRedisHarness(t)
	rules, err := Build([]RuleConfig{
		{
			Name:      "per-key-min",
			Algorithm: "redis_window",
			Limit:     60,
			Window:    time.Minute,
			KeyBy:     []string{"api_key"},
		},
	}, client)
	require.NoError(t, err)
	require.Len(t, rules, 1)
	assert.Equal(t, []KeyBy{KeyByAPIKey}, rules[0].KeyBy)
}

func TestBuild_RedisWindow_NoClientFails(t *testing.T) {
	_, err := Build([]RuleConfig{
		{Name: "x", Algorithm: "redis_window", Limit: 5, Window: time.Second},
	}, nil)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "redis_window requires Redis")
}

func TestBuild_UnknownAlgorithm(t *testing.T) {
	_, err := Build([]RuleConfig{
		{Name: "bogus", Algorithm: "leaky_bucket"},
	}, nil)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "unknown algorithm")
}

func TestBuild_DuplicateName(t *testing.T) {
	_, err := Build([]RuleConfig{
		{Name: "x", Algorithm: "memory_bucket", Rate: "10/s"},
		{Name: "x", Algorithm: "memory_bucket", Rate: "20/s"},
	}, nil)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "duplicate name")
}

func TestBuild_MissingName(t *testing.T) {
	_, err := Build([]RuleConfig{{Algorithm: "memory_bucket", Rate: "10/s"}}, nil)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "name is required")
}

func TestBuild_UnknownKeyBy(t *testing.T) {
	_, err := Build([]RuleConfig{
		{Name: "x", Algorithm: "memory_bucket", Rate: "10/s", KeyBy: []string{"ip"}},
	}, nil)
	require.Error(t, err)
	assert.Contains(t, err.Error(), `unknown key_by "ip"`)
}

func TestBuild_AggregatesErrors(t *testing.T) {
	// Three problems in the same load → all surfaced.
	_, err := Build([]RuleConfig{
		{Name: ""},
		{Name: "ok", Algorithm: "memory_bucket", Rate: "junk"},
		{Name: "x", Algorithm: "redis_window", Limit: 5, Window: time.Second},
	}, nil)
	require.Error(t, err)
	msg := err.Error()
	assert.Contains(t, msg, "name is required")
	assert.Contains(t, msg, "rate")
	assert.Contains(t, msg, "redis_window requires Redis")
}

func TestParseRate(t *testing.T) {
	cases := []struct {
		in   string
		want float64
	}{
		{"100/s", 100},
		{"60/m", 1},
		{"3600/h", 1},
		{"1/s", 1},
	}
	for _, c := range cases {
		got, err := parseRate(c.in)
		require.NoError(t, err, "in=%q", c.in)
		assert.InDelta(t, c.want, float64(got), 1e-9, "in=%q", c.in)
	}
}

func TestParseRate_Errors(t *testing.T) {
	for _, in := range []string{"", "100", "x/s", "100/d", "0/s", "-1/s", "100//s"} {
		_, err := parseRate(in)
		assert.Error(t, err, "expected error for %q", in)
	}
}

func TestBuild_IntegratesWithMulti(t *testing.T) {
	// End-to-end: build two rules (memory + redis), wrap in Multi, verify
	// first-deny-wins still functions.
	client, _ := newRedisHarness(t)

	rules, err := Build([]RuleConfig{
		{Name: "global-rps", Algorithm: "memory_bucket", Rate: "1000/s", Burst: 1000},
		{Name: "per-key-min", Algorithm: "redis_window", Limit: 2, Window: time.Second, KeyBy: []string{"api_key"}},
	}, client)
	require.NoError(t, err)

	m := NewMulti(rules...)
	ctx := context.Background()

	d, err := m.Check(ctx, KeyContext{APIKeyID: "k1"}, 1)
	require.NoError(t, err)
	assert.True(t, d.Allowed)

	d, _ = m.Check(ctx, KeyContext{APIKeyID: "k1"}, 1)
	assert.True(t, d.Allowed)

	d, _ = m.Check(ctx, KeyContext{APIKeyID: "k1"}, 1)
	assert.False(t, d.Allowed, "third request should hit redis_window limit (2)")
	assert.Equal(t, "per-key-min", d.Rule)

	// Different key — independent.
	d, _ = m.Check(ctx, KeyContext{APIKeyID: "k2"}, 1)
	assert.True(t, d.Allowed)
}
