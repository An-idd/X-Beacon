package server

import (
	"errors"
	"net/http"

	"github.com/An-idd/x-beacon/internal/provider"
)

// errorEnvelope is the OpenAI-shaped error body the gateway returns to
// clients. The shape matches OpenAI's so client SDKs surface the message
// naturally, even when the upstream is Anthropic or DeepSeek.
type errorEnvelope struct {
	Error errorBody `json:"error"`
}

type errorBody struct {
	Type    string `json:"type"`
	Code    string `json:"code,omitempty"`
	Message string `json:"message"`
	Param   string `json:"param,omitempty"`
	ReqID   string `json:"req_id,omitempty"`
}

// mappedError describes the gateway's translation of an upstream/internal
// error into a client-visible HTTP response. Callers (chat handler,
// streaming handler in 3.5) use it to populate the response.
type mappedError struct {
	Status     int
	Type       string // OpenAI error.type value
	Code       string // sub-classifier (e.g. "model_not_found")
	Message    string
	RetryAfter int // seconds, 0 = no header
}

// mapProviderError translates a provider error into an HTTP response shape.
// The default fallback is 500 internal_error so unknown errors never leak
// through as 200.
func mapProviderError(err error) mappedError {
	switch {
	case errors.Is(err, provider.ErrAuth):
		// Upstream rejected the gateway's API key — a gateway misconfiguration,
		// not a client problem. 502 communicates "the gateway can't talk to
		// its upstream" without confusing clients into rotating their own key.
		return mappedError{Status: http.StatusBadGateway, Type: "upstream_error", Code: "upstream_auth_failed",
			Message: "Upstream provider rejected the gateway's credentials"}

	case errors.Is(err, provider.ErrRateLimited):
		m := mappedError{Status: http.StatusTooManyRequests, Type: "rate_limit_error",
			Message: "Upstream rate limit exceeded"}
		var ue *provider.UpstreamError
		if errors.As(err, &ue) {
			if ue.RetryAfter > 0 {
				m.RetryAfter = int(ue.RetryAfter.Seconds())
			}
			if ue.Message != "" {
				m.Message = ue.Message
			}
		}
		return m

	case errors.Is(err, provider.ErrContextLength):
		return mappedError{Status: http.StatusBadRequest, Type: "invalid_request_error",
			Code: "context_length_exceeded", Message: messageOrDefault(err, "Request exceeds the model's context length")}

	case errors.Is(err, provider.ErrInvalidRequest):
		return mappedError{Status: http.StatusBadRequest, Type: "invalid_request_error",
			Message: messageOrDefault(err, "Invalid request")}

	case errors.Is(err, provider.ErrTimeout):
		return mappedError{Status: http.StatusGatewayTimeout, Type: "timeout_error",
			Message: messageOrDefault(err, "Upstream request timed out")}

	case errors.Is(err, provider.ErrUnavailable):
		return mappedError{Status: http.StatusServiceUnavailable, Type: "service_unavailable",
			Message: messageOrDefault(err, "Upstream provider is unavailable")}

	case errors.Is(err, provider.ErrUpstream):
		return mappedError{Status: http.StatusBadGateway, Type: "upstream_error",
			Message: messageOrDefault(err, "Upstream provider returned an error")}

	default:
		// Unrecognized error: do not leak internals. Log via caller.
		return mappedError{Status: http.StatusInternalServerError, Type: "internal_error",
			Message: "Internal server error"}
	}
}

// messageOrDefault prefers the UpstreamError.Message (which has been
// vetted for prompt-leakage at provider construction) over the
// underlying err.Error() (which would surface raw upstream payloads).
func messageOrDefault(err error, fallback string) string {
	var ue *provider.UpstreamError
	if errors.As(err, &ue) && ue.Message != "" {
		return ue.Message
	}
	return fallback
}

// writeError serializes a mappedError to w with the configured status,
// optional Retry-After header, and the request_id (if any) for client-side
// correlation.
func writeError(w http.ResponseWriter, m mappedError, reqID string) {
	if m.RetryAfter > 0 {
		w.Header().Set("Retry-After", itoa(m.RetryAfter))
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(m.Status)
	body := errorEnvelope{Error: errorBody{
		Type:    m.Type,
		Code:    m.Code,
		Message: m.Message,
		ReqID:   reqID,
	}}
	_ = jsonEncode(w, body)
}
