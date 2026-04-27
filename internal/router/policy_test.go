package router

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
)

func TestDefaultPolicy(t *testing.T) {
	p := DefaultPolicy()
	assert.Equal(t, 2, p.MaxRetries)
	assert.Equal(t, 30*time.Second, p.MaxTotal)
	assert.Equal(t, 100*time.Millisecond, p.BaseBackoff)
	assert.Equal(t, 5*time.Second, p.MaxBackoff)
}

func TestBackoffDuration_Envelope(t *testing.T) {
	p := DefaultPolicy()
	// randFloat=1.0 (clamped to ~1) yields the full envelope.
	tests := []struct {
		attempt  int
		envelope time.Duration
	}{
		{1, 100 * time.Millisecond},
		{2, 200 * time.Millisecond},
		{3, 400 * time.Millisecond},
		{4, 800 * time.Millisecond},
		{5, 1600 * time.Millisecond},
		{6, 3200 * time.Millisecond},
		{7, 5 * time.Second}, // capped at MaxBackoff
		{8, 5 * time.Second},
		{20, 5 * time.Second}, // overflow protected
	}
	for _, tc := range tests {
		got := p.backoffDuration(tc.attempt, 0, 0.999999)
		// We allow a 1ms slop because of float64 → int64 truncation.
		assert.InDelta(t, int64(tc.envelope), int64(got), float64(time.Millisecond),
			"attempt=%d envelope=%v got=%v", tc.attempt, tc.envelope, got)
	}
}

func TestBackoffDuration_FullJitter(t *testing.T) {
	p := DefaultPolicy()
	// attempt=3 → envelope=400ms
	// randFloat=0 → 0
	// randFloat=0.5 → 200ms
	// randFloat=1 (clamped 0.999999) → ~400ms
	assert.Equal(t, time.Duration(0), p.backoffDuration(3, 0, 0))
	assert.Equal(t, 200*time.Millisecond, p.backoffDuration(3, 0, 0.5))

	full := p.backoffDuration(3, 0, 1.0)
	assert.GreaterOrEqual(t, full, 399*time.Millisecond)
	assert.LessOrEqual(t, full, 400*time.Millisecond)
}

func TestBackoffDuration_RetryAfterTrumpsJitter(t *testing.T) {
	p := DefaultPolicy()
	// Upstream said wait 7s. We honor it verbatim, ignoring envelope and jitter.
	got := p.backoffDuration(1, 7*time.Second, 0.5)
	assert.Equal(t, 7*time.Second, got)

	// Even if Retry-After exceeds MaxBackoff, it wins. The MaxTotal budget
	// check in the retry loop is what protects us from waiting forever.
	got = p.backoffDuration(2, 60*time.Second, 0.0)
	assert.Equal(t, 60*time.Second, got)
}

func TestBackoffDuration_AttemptZero(t *testing.T) {
	p := DefaultPolicy()
	// attempt=0 is nonsensical (no retries yet); return 0 rather than negative.
	assert.Equal(t, time.Duration(0), p.backoffDuration(0, 0, 0.5))
}

func TestBackoffDuration_RandClamp(t *testing.T) {
	p := DefaultPolicy()
	// Out-of-range randFloat is clamped instead of producing garbage.
	assert.Equal(t, time.Duration(0), p.backoffDuration(1, 0, -0.5))
	assert.GreaterOrEqual(t, p.backoffDuration(1, 0, 1.5), 99*time.Millisecond)
}
