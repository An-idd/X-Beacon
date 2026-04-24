package openai

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"time"

	"github.com/An-idd/x-beacon/internal/provider"
)

// openaiErrorPayload is the inner object of OpenAI's error envelope.
// Named separately so the streaming path can embed it without the extra
// "error" wrapper (mid-stream chunks carry the envelope in the same shape).
type openaiErrorPayload struct {
	Message string `json:"message"`
	Type    string `json:"type"`
	Code    string `json:"code"`
	Param   string `json:"param,omitempty"`
}

// apiError matches OpenAI's standard error response body shape:
//
//	{"error": {"message": "...", "type": "...", "code": "..."}}
type apiError struct {
	Error openaiErrorPayload `json:"error"`
}

// maxEmbeddedBodyLen caps fallback error messages constructed from raw
// response bodies (when the body isn't valid JSON).
const maxEmbeddedBodyLen = 200

// mapHTTPError converts an OpenAI HTTP error response into a provider
// sentinel-backed UpstreamError. The body is best-effort parsed; malformed
// bodies degrade gracefully to a truncated raw-string message.
func mapHTTPError(providerName string, status int, body []byte, header http.Header) error {
	var ae apiError
	_ = json.Unmarshal(body, &ae) // ignore decode error — we'll fall back to raw body

	sentinel := mapStatusToSentinel(status, ae.Error.Code)

	msg := ae.Error.Message
	if msg == "" {
		msg = string(body)
		if len(msg) > maxEmbeddedBodyLen {
			msg = msg[:maxEmbeddedBodyLen] + "..."
		}
	}

	ue := provider.NewUpstreamError(providerName, sentinel, status, msg)
	if status == http.StatusTooManyRequests {
		ue.RetryAfter = parseRetryAfter(header.Get("Retry-After"))
	}
	return ue
}

// mapStatusToSentinel is the canonical mapping table from HTTP status (+ the
// optional OpenAI error.code for 400) to our sentinel errors.
func mapStatusToSentinel(status int, code string) error {
	switch status {
	case http.StatusUnauthorized, http.StatusForbidden:
		return provider.ErrAuth
	case http.StatusTooManyRequests:
		return provider.ErrRateLimited
	case http.StatusBadRequest:
		if code == "context_length_exceeded" {
			return provider.ErrContextLength
		}
		return provider.ErrInvalidRequest
	case http.StatusNotFound, http.StatusUnprocessableEntity:
		return provider.ErrInvalidRequest
	case http.StatusRequestTimeout:
		return provider.ErrTimeout
	case http.StatusServiceUnavailable:
		return provider.ErrUnavailable
	}
	if status >= 500 && status < 600 {
		return provider.ErrUpstream
	}
	// Unknown 4xx: treat as upstream to avoid silently swallowing.
	return provider.ErrUpstream
}

// parseRetryAfter accepts the two forms allowed by RFC 7231: delta-seconds
// or an HTTP-date. Returns 0 on any parse failure or past-date.
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

// mapRequestError handles errors from http.Client.Do — mostly network
// failures and ctx-driven termination. It preserves ctx.Canceled semantics
// (non-retryable) and maps ctx.DeadlineExceeded to ErrTimeout (retryable).
func mapRequestError(providerName string, err error) error {
	if errors.Is(err, context.Canceled) {
		return err
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return provider.NewUpstreamError(providerName, provider.ErrTimeout, 0, "request deadline exceeded")
	}
	return provider.NewUpstreamError(providerName, provider.ErrUnavailable, 0, err.Error())
}
