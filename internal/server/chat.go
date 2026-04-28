package server

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"time"

	"go.uber.org/zap"

	"github.com/An-idd/x-beacon/internal/auth"
	"github.com/An-idd/x-beacon/internal/billing"
	"github.com/An-idd/x-beacon/internal/observability"
	"github.com/An-idd/x-beacon/internal/provider"
	"github.com/An-idd/x-beacon/internal/router"
	"github.com/An-idd/x-beacon/internal/server/middleware"
	"github.com/An-idd/x-beacon/pkg/tokenizer"
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
// Non-streaming flow:
//
//	parse → validate → router.ResolveModel (400 if unknown) →
//	  router.ChatCompletion (retry + failover + breaker) → write JSON →
//	  enqueue billing event
//
// Streaming flow (req.Stream == true) delegates to handleChatStream which
// uses router.ChatCompletionStream — same retry/failover semantics, gated
// to "before first chunk", with the billing event emitted after the
// stream terminates.
//
// tokenizer / billing may be nil; the handler degrades gracefully (no
// token attribution / no billing rows) so dev-mode without DB still
// boots and serves traffic.
func chatCompletionsHandler(rtr *router.Router, tk *tokenizer.Selector, bill *billing.Worker, metrics *observability.Metrics, logger *zap.Logger) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		reqID := middleware.RequestIDFrom(r.Context())
		started := time.Now()

		req, mapped, ok := readChatRequest(r)
		if !ok {
			writeError(w, mapped, reqID)
			metrics.ObserveRequest("", "", mapped.Status, time.Since(started).Seconds())
			return
		}

		// Pre-flight: surface 400 model_not_found before invoking the
		// router so we don't conflate "unknown model" with "all upstreams
		// failed". The router itself also surfaces no-chain errors, but
		// here we get a clean 4xx.
		if _, err := rtr.ResolveModel(req.Model); err != nil {
			writeError(w, mappedError{
				Status:  http.StatusBadRequest,
				Type:    "invalid_request_error",
				Code:    "model_not_found",
				Message: "Model " + req.Model + " is not configured on this gateway",
			}, reqID)
			metrics.ObserveRequest("", req.Model, http.StatusBadRequest, time.Since(started).Seconds())
			return
		}

		if req.Stream {
			handleChatStream(w, r, rtr, tk, bill, metrics, req, started, logger, reqID)
			return
		}

		resp, err := rtr.ChatCompletion(r.Context(), req)
		if err != nil {
			m := mapProviderError(err)
			logger.Warn("chat completion failed",
				zap.String("req_id", reqID),
				zap.String("model", req.Model),
				zap.Int("status", m.Status),
				zap.Error(err))
			writeError(w, m, reqID)
			metrics.ObserveRequest("", req.Model, m.Status, time.Since(started).Seconds())
			emitBillingEvent(bill, billing.Event{
				StartedAt: started,
				RequestID: reqID,
				APIKeyID:  apiKeyIDFrom(r),
				Model:     req.Model,
				Status:    m.Status,
				LatencyMs: int(time.Since(started).Milliseconds()),
				ErrorCode: m.Code,
			})
			return
		}

		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		if err := jsonEncode(w, resp); err != nil {
			logger.Error("failed to encode chat response",
				zap.String("req_id", reqID),
				zap.Error(err))
		}

		// Build the billing event from the upstream's Usage. When Usage
		// is missing (rare for non-stream), fall back to tokenizer
		// estimates so the row is still useful for QPS / latency.
		promptTok, completionTok := tokenCounts(tk, req, resp)
		metrics.ObserveRequest(resp.Provider, req.Model, http.StatusOK, time.Since(started).Seconds())
		metrics.AddTokens(resp.Provider, req.Model, promptTok, completionTok)
		emitBillingEvent(bill, billing.Event{
			StartedAt:        started,
			RequestID:        reqID,
			APIKeyID:         apiKeyIDFrom(r),
			Provider:         resp.Provider,
			Model:            req.Model,
			PromptTokens:     promptTok,
			CompletionTokens: completionTok,
			LatencyMs:        int(time.Since(started).Milliseconds()),
			Status:           http.StatusOK,
			Streamed:         false,
		})
	}
}

// tokenCounts extracts (prompt, completion) tokens for billing. Prefers
// upstream-supplied Usage; falls back to local tokenizer estimates when
// fields are zero or absent.
func tokenCounts(tk *tokenizer.Selector, req *provider.ChatRequest, resp *provider.ChatResponse) (int, int) {
	var prompt, completion int
	if resp != nil && resp.Usage != nil {
		prompt = resp.Usage.PromptTokens
		completion = resp.Usage.CompletionTokens
	}
	if tk == nil {
		return prompt, completion
	}
	t := tk.For(req.Model)
	if prompt == 0 {
		prompt = t.CountMessages(req.Messages)
	}
	if completion == 0 && resp != nil {
		// Sum content across choices; usually 1 choice but n>1 is allowed.
		for _, ch := range resp.Choices {
			completion += t.CountText(ch.Message.Content)
		}
	}
	return prompt, completion
}

// emitBillingEvent enqueues an event when the worker is wired. A nil
// worker (dev mode without DB) is a no-op — events are dropped on the
// floor rather than buffered.
func emitBillingEvent(bill *billing.Worker, ev billing.Event) {
	if bill == nil {
		return
	}
	bill.Enqueue(ev)
}

// apiKeyIDFrom pulls the Principal id from the request context. Returns
// "" when auth is disabled (dev mode) or the middleware didn't set one.
func apiKeyIDFrom(r *http.Request) string {
	if p := auth.PrincipalFrom(r.Context()); p != nil {
		return p.ID
	}
	return ""
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
