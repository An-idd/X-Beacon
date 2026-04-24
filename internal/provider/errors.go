package provider

import (
	"errors"
	"fmt"
	"time"
)

// Package-level sentinel errors. Match with errors.Is. Each provider adapter
// must translate its native error codes into one of these before returning.
var (
	ErrAuth           = errors.New("provider: authentication failed")
	ErrRateLimited    = errors.New("provider: rate limited")
	ErrContextLength  = errors.New("provider: context length exceeded")
	ErrInvalidRequest = errors.New("provider: invalid request")
	ErrUpstream       = errors.New("provider: upstream error")
	ErrUnavailable    = errors.New("provider: service unavailable")
	ErrTimeout        = errors.New("provider: timeout")
)

// UpstreamError carries provider-specific context alongside a sentinel.
// Construct with NewUpstreamError; inspect with errors.As.
//
//	var ue *provider.UpstreamError
//	if errors.As(err, &ue) { ... }          // structured extraction
//	if errors.Is(err, provider.ErrRateLimited) { ... }  // category match
type UpstreamError struct {
	Provider   string        // provider name (e.g. "openai-primary")
	StatusCode int           // HTTP status from upstream, 0 if N/A
	RetryAfter time.Duration // parsed Retry-After header, 0 if absent
	Message    string        // provider-supplied error message (sanitized)
	Sentinel   error         // one of the package sentinels; required
}

func (e *UpstreamError) Error() string {
	switch {
	case e.StatusCode > 0 && e.Message != "":
		return fmt.Sprintf("provider %q: %s (status=%d): %s", e.Provider, e.Sentinel, e.StatusCode, e.Message)
	case e.StatusCode > 0:
		return fmt.Sprintf("provider %q: %s (status=%d)", e.Provider, e.Sentinel, e.StatusCode)
	case e.Message != "":
		return fmt.Sprintf("provider %q: %s: %s", e.Provider, e.Sentinel, e.Message)
	default:
		return fmt.Sprintf("provider %q: %s", e.Provider, e.Sentinel)
	}
}

// Unwrap enables errors.Is matching against the embedded sentinel.
func (e *UpstreamError) Unwrap() error { return e.Sentinel }

// NewUpstreamError constructs an UpstreamError. Sentinel must be one of
// the package-level sentinels (non-nil); message may be empty.
func NewUpstreamError(providerName string, sentinel error, statusCode int, message string) *UpstreamError {
	return &UpstreamError{
		Provider:   providerName,
		Sentinel:   sentinel,
		StatusCode: statusCode,
		Message:    message,
	}
}

// IsRetryable reports whether the caller should retry the operation (with
// backoff). Rate limiting, transient upstream errors, unavailability, and
// timeouts are retryable; authentication and client-side errors are not.
func IsRetryable(err error) bool {
	switch {
	case errors.Is(err, ErrRateLimited),
		errors.Is(err, ErrUpstream),
		errors.Is(err, ErrUnavailable),
		errors.Is(err, ErrTimeout):
		return true
	default:
		return false
	}
}
