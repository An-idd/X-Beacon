package server

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"time"

	"go.uber.org/zap"

	"github.com/An-idd/x-beacon/internal/provider"
	"github.com/An-idd/x-beacon/internal/provider/registry"
	"github.com/An-idd/x-beacon/internal/server/middleware"
)

// maxRequestBytes caps inbound /v1/chat/completions bodies. 1 MiB easily
// fits any reasonable chat history while bounding memory exposure to
// malformed/abusive clients. Promoted from the Week 1 carry-over note in
// openai/chat.go (which deferred body limiting to "router does it").
const maxRequestBytes = 1 << 20

// streamHeartbeatInterval is how often the SSE writer emits a comment
// keep-alive while a stream is in flight. 15s threads the needle between
// nginx's typical 60s idle timeout and burning bandwidth on quiet streams.
const streamHeartbeatInterval = 15 * time.Second

// chatCompletionsHandler returns the /v1/chat/completions handler.
//
// Non-streaming flow (3.4):
//
//	parse → validate → registry.ResolveModel → provider.ChatCompletion → write JSON
//
// Streaming flow (req.Stream == true) returns 501 in this step and is
// implemented by Step 3.5's SSE writer.
func chatCompletionsHandler(reg *registry.Registry, logger *zap.Logger) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		reqID := middleware.RequestIDFrom(r.Context())

		req, mapped, ok := readChatRequest(r)
		if !ok {
			writeError(w, mapped, reqID)
			return
		}

		p, err := reg.ResolveModel(req.Model)
		if err != nil {
			writeError(w, mappedError{
				Status:  http.StatusBadRequest,
				Type:    "invalid_request_error",
				Code:    "model_not_found",
				Message: "Model " + req.Model + " is not configured on this gateway",
			}, reqID)
			return
		}

		if req.Stream {
			handleChatStream(w, r, p, req, logger, reqID)
			return
		}

		resp, err := p.ChatCompletion(r.Context(), req)
		if err != nil {
			m := mapProviderError(err)
			// Log at error level for 5xx, warn for 4xx — same convention as
			// the Logging middleware. Prompt content is not logged; only
			// shape + provider name + status.
			logger.Warn("chat completion failed",
				zap.String("req_id", reqID),
				zap.String("provider", p.Name()),
				zap.String("model", req.Model),
				zap.Int("status", m.Status),
				zap.Error(err))
			writeError(w, m, reqID)
			return
		}

		w.Header().Set("Content-Type", "application/json")
		// Default 200; explicit WriteHeader so the Logging middleware sees it
		// even when Encode happens to flush at the same instant.
		w.WriteHeader(http.StatusOK)
		if err := jsonEncode(w, resp); err != nil {
			// At this point status is already written; can't change it.
			// Just log and let the client see a truncated body.
			logger.Error("failed to encode chat response",
				zap.String("req_id", reqID),
				zap.Error(err))
		}
	}
}

// readChatRequest reads, decodes, and shallow-validates the request body.
// Returns ok=false with a populated mappedError when the request is
// malformed or oversize; the caller is expected to writeError and return.
func readChatRequest(r *http.Request) (*provider.ChatRequest, mappedError, bool) {
	if r.Body == nil {
		return nil, mappedError{
			Status: http.StatusBadRequest, Type: "invalid_request_error",
			Message: "Empty request body",
		}, false
	}

	// MaxBytesReader returns a *http.MaxBytesError on overflow, distinct
	// from io.EOF/UnexpectedEOF, so we can map 413 specifically.
	body := http.MaxBytesReader(nil, r.Body, maxRequestBytes)
	defer body.Close()

	raw, err := io.ReadAll(body)
	if err != nil {
		var maxErr *http.MaxBytesError
		if errors.As(err, &maxErr) {
			return nil, mappedError{
				Status: http.StatusRequestEntityTooLarge, Type: "invalid_request_error",
				Code: "request_too_large", Message: "Request body exceeds 1 MiB",
			}, false
		}
		return nil, mappedError{
			Status: http.StatusBadRequest, Type: "invalid_request_error",
			Message: "Failed to read request body",
		}, false
	}

	if len(raw) == 0 {
		return nil, mappedError{
			Status: http.StatusBadRequest, Type: "invalid_request_error",
			Message: "Empty request body",
		}, false
	}

	var req provider.ChatRequest
	if err := json.Unmarshal(raw, &req); err != nil {
		return nil, mappedError{
			Status: http.StatusBadRequest, Type: "invalid_request_error",
			Message: "Malformed JSON: " + err.Error(),
		}, false
	}

	if req.Model == "" {
		return nil, mappedError{
			Status: http.StatusBadRequest, Type: "invalid_request_error",
			Code: "missing_model", Message: "Field 'model' is required",
		}, false
	}
	if len(req.Messages) == 0 {
		return nil, mappedError{
			Status: http.StatusBadRequest, Type: "invalid_request_error",
			Code: "missing_messages", Message: "Field 'messages' must contain at least one message",
		}, false
	}

	return &req, mappedError{}, true
}
