package server

import (
	"encoding/json"
	"net/http"

	"go.uber.org/zap"

	"github.com/An-idd/x-beacon/internal/provider"
	"github.com/An-idd/x-beacon/internal/router"
	"github.com/An-idd/x-beacon/internal/server/sse"
)

// handleChatStream owns the streaming branch of /v1/chat/completions.
//
// Two failure boundaries are distinguished:
//
//	pre-stream  — ChatCompletionStream returns (nil, err): the gateway has
//	              not yet committed to SSE response headers. Surface as a
//	              JSON HTTP error (same shape as the non-streaming path).
//
//	mid-stream  — channel emits StreamEvent{Err}: SSE response is already
//	              flowing. Surface as `data: {"error":...}\n\n` and stop;
//	              do NOT emit `[DONE]` (matches OpenAI's observed semantics).
//
// On a clean end-of-stream the handler emits `data: [DONE]\n\n`. On client
// disconnect (r.Context().Done) the handler returns; the provider goroutine
// is blocked on the same context and exits, draining the channel naturally.
func handleChatStream(
	w http.ResponseWriter,
	r *http.Request,
	rtr *router.Router,
	req *provider.ChatRequest,
	logger *zap.Logger,
	reqID string,
) {
	ch, err := rtr.ChatCompletionStream(r.Context(), req)
	if err != nil {
		m := mapProviderError(err)
		logger.Warn("chat stream rejected pre-stream",
			zap.String("req_id", reqID),
			zap.String("model", req.Model),
			zap.Int("status", m.Status),
			zap.Error(err))
		writeError(w, m, reqID)
		return
	}

	sw, sseErr := sse.New(w)
	if sseErr != nil {
		// Real http.Server writers always implement Flusher; this branch
		// only triggers in tests with a non-Flusher recorder.
		writeError(w, mappedError{
			Status:  http.StatusInternalServerError,
			Type:    "internal_error",
			Message: "Server does not support streaming",
		}, reqID)
		return
	}

	stopHB := sw.StartHeartbeat(r.Context(), streamHeartbeatInterval)
	defer stopHB()

	for ev := range ch {
		if ev.Err != nil {
			emitStreamError(sw, ev.Err, req.Model, reqID, logger)
			return
		}

		data, err := json.Marshal(ev.Chunk)
		if err != nil {
			logger.Error("encode stream chunk",
				zap.String("req_id", reqID),
				zap.String("model", req.Model),
				zap.Error(err))
			// Same envelope as a mid-stream error: client gets one bad event
			// instead of a silently truncated stream.
			emitStreamError(sw, err, req.Model, reqID, logger)
			return
		}

		if err := sw.WriteData(data); err != nil {
			// Almost always "client disconnected". ctx will be canceled
			// shortly which exits the producer; nothing more to do here.
			logger.Debug("stream write failed; ending",
				zap.String("req_id", reqID),
				zap.Error(err))
			return
		}
	}

	// Channel closed naturally. If ctx was canceled (client gone), skip the
	// terminal marker — the connection is dead anyway.
	if r.Context().Err() != nil {
		return
	}
	_ = sw.WriteData(doneMarker)
}

// doneMarker is the OpenAI-canonical end-of-stream sentinel. Written as
// raw bytes (no JSON escaping) because clients match the literal string.
var doneMarker = []byte("[DONE]")

// emitStreamError serializes err into an OpenAI-shaped SSE error event.
// The message is sourced from the upstream's structured error body when
// available — never the raw err.Error() — to avoid leaking prompts that
// some providers echo back inside their error strings.
//
// Provider attribution is logged by the router layer; this function
// records only the model + req_id since the handler no longer holds a
// direct provider reference.
func emitStreamError(
	sw *sse.Writer,
	err error,
	model string,
	reqID string,
	logger *zap.Logger,
) {
	m := mapProviderError(err)
	envelope := errorEnvelope{Error: errorBody{
		Type:    m.Type,
		Code:    m.Code,
		Message: m.Message,
		ReqID:   reqID,
	}}

	body, marshalErr := json.Marshal(envelope)
	if marshalErr != nil {
		// errorEnvelope is composed of strings; marshal can't realistically
		// fail. Fall back to a hand-rolled minimal payload to keep the
		// invariant "every stream ends with either [DONE] or an error event".
		body = []byte(`{"error":{"type":"internal_error","message":"failed to encode error"}}`)
	}

	if writeErr := sw.WriteData(body); writeErr != nil {
		logger.Debug("failed to write stream error frame",
			zap.String("req_id", reqID),
			zap.Error(writeErr))
	}

	logger.Warn("chat stream upstream error",
		zap.String("req_id", reqID),
		zap.String("model", model),
		zap.Int("status", m.Status),
		zap.Error(err))
}
