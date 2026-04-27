package router

import "time"

// RetryPolicy controls how the router retries a single provider call when it
// returns a retryable error (provider.IsRetryable). Zero value is unusable;
// always start from DefaultPolicy() and tweak fields.
//
// Two budget knobs run AND-style: the loop stops the first time either is
// exhausted. Both default to bounded values to prevent quick-fail storms
// (count) and slow-fail backlog (time) from compounding.
type RetryPolicy struct {
	// MaxRetries is the maximum number of retry attempts BEYOND the first
	// call. MaxRetries=2 means up to 3 total calls (1 + 2 retries).
	// 0 disables retry entirely.
	MaxRetries int

	// MaxTotal caps the wall-clock budget for the whole sequence including
	// the first call and all backoffs. 0 means unlimited (only MaxRetries
	// applies). Honored by aborting before a sleep that would overrun.
	MaxTotal time.Duration

	// BaseBackoff is the t in `t * 2^(attempt-1)` (full-jitter envelope).
	// Defaults to 100ms in DefaultPolicy.
	BaseBackoff time.Duration

	// MaxBackoff caps the envelope before jitter is applied. Without a cap,
	// attempt 8 with base=100ms reaches ~25s — usually well past any sane
	// MaxTotal. Defaults to 5s.
	MaxBackoff time.Duration
}

// DefaultPolicy returns the carry-over decision values from Week 6:
// 2 retries / 30s total / 100ms base / 5s cap.
func DefaultPolicy() RetryPolicy {
	return RetryPolicy{
		MaxRetries:  2,
		MaxTotal:    30 * time.Second,
		BaseBackoff: 100 * time.Millisecond,
		MaxBackoff:  5 * time.Second,
	}
}

// backoffDuration returns the wall-clock delay before retry attempt n
// (1-indexed; attempt=1 is the first retry, i.e. after the original call
// failed once). Pure function; no side effects.
//
// Algorithm — "full jitter" exponential
// (https://aws.amazon.com/blogs/architecture/exponential-backoff-and-jitter/):
//
//	envelope = min(MaxBackoff, BaseBackoff * 2^(attempt-1))
//	delay    = randFloat * envelope             // randFloat ∈ [0, 1)
//
// retryAfter takes precedence: when the upstream signaled `Retry-After`
// (parsed into provider.UpstreamError.RetryAfter), we trust it verbatim and
// skip jitter — fighting the upstream's explicit signal just gets us
// rate-limited again on the next attempt.
func (p RetryPolicy) backoffDuration(attempt int, retryAfter time.Duration, randFloat float64) time.Duration {
	if retryAfter > 0 {
		return retryAfter
	}
	if attempt < 1 {
		return 0
	}
	// Compute BaseBackoff * 2^(attempt-1) with overflow / cap protection.
	envelope := p.BaseBackoff
	for i := 1; i < attempt; i++ {
		envelope *= 2
		if envelope <= 0 || envelope >= p.MaxBackoff {
			envelope = p.MaxBackoff
			break
		}
	}
	if envelope > p.MaxBackoff {
		envelope = p.MaxBackoff
	}
	if randFloat < 0 {
		randFloat = 0
	}
	if randFloat >= 1 {
		randFloat = 0.999999
	}
	return time.Duration(randFloat * float64(envelope))
}
