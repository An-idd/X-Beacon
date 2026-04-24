package anthropic

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"time"

	"github.com/An-idd/x-beacon/internal/provider"
)

// apiError matches Anthropic's error response body shape:
//
//	{"type": "error", "error": {"type": "rate_limit_error", "message": "..."}}
type apiError struct {
	Type  string `json:"type"` // "error"
	Error struct {
		Type    string `json:"type"`
		Message string `json:"message"`
	} `json:"error"`
}

const maxEmbeddedBodyLen = 200

// mapHTTPError converts an Anthropic HTTP error response into a provider
// sentinel-backed UpstreamError. The Anthropic error.type field takes
// precedence over HTTP status for classification (e.g. 529 overloaded_error
// → ErrUnavailable), because the status/type pairing is the vendor's
// authoritative signal.
func mapHTTPError(providerName string, status int, body []byte, header http.Header) error {
	var ae apiError
	_ = json.Unmarshal(body, &ae) // best-effort

	sentinel := mapToSentinel(status, ae.Error.Type)

	msg := ae.Error.Message
	if msg == "" {
		msg = string(body)
		if len(msg) > maxEmbeddedBodyLen {
			msg = msg[:maxEmbeddedBodyLen] + "..."
		}
	}

	ue := provider.NewUpstreamError(providerName, sentinel, status, msg)
	// Anthropic returns Retry-After on 429 and sometimes 529 overloaded.
	if status == http.StatusTooManyRequests || ae.Error.Type == "overloaded_error" {
		ue.RetryAfter = parseRetryAfter(header.Get("Retry-After"))
	}
	return ue
}

// mapToSentinel first tries Anthropic's vendor-specific error.type, then
// falls back to HTTP status. Unknown combinations map to ErrUpstream.
func mapToSentinel(status int, errorType string) error {
	switch errorType {
	case "authentication_error", "permission_error":
		return provider.ErrAuth
	case "rate_limit_error":
		return provider.ErrRateLimited
	case "overloaded_error":
		return provider.ErrUnavailable
	case "invalid_request_error", "not_found_error":
		return provider.ErrInvalidRequest
	case "request_too_large":
		return provider.ErrContextLength
	case "api_error":
		return provider.ErrUpstream
	}

	switch status {
	case http.StatusUnauthorized, http.StatusForbidden:
		return provider.ErrAuth
	case http.StatusTooManyRequests:
		return provider.ErrRateLimited
	case http.StatusBadRequest, http.StatusNotFound, http.StatusUnprocessableEntity:
		return provider.ErrInvalidRequest
	case http.StatusRequestTimeout:
		return provider.ErrTimeout
	case http.StatusServiceUnavailable:
		return provider.ErrUnavailable
	case 529: // Anthropic's own "Overloaded"
		return provider.ErrUnavailable
	}
	if status >= 500 && status < 600 {
		return provider.ErrUpstream
	}
	return provider.ErrUpstream
}

// parseRetryAfter mirrors openai.parseRetryAfter: seconds OR HTTP-date.
func parseRetryAfter(v string) time.Duration {
	if v == "" {
		return 0
	}
	if secs, err := strconv.Atoi(v); err == nil && secs >= 0 {
		return time.Duration(secs) * time.Second
	}
	if t, err := http.ParseTime(v); err == nil {
		if d := time.Until(t); d > 0 {
			return d
		}
	}
	return 0
}

// mapRequestError handles errors returned by http.Client.Do.
// Mirrors openai.mapRequestError for consistent retry semantics.
func mapRequestError(providerName string, err error) error {
	if errors.Is(err, context.Canceled) {
		return err
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return provider.NewUpstreamError(providerName, provider.ErrTimeout, 0, "request deadline exceeded")
	}
	return provider.NewUpstreamError(providerName, provider.ErrUnavailable, 0, err.Error())
}
