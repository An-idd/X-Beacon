package server

import (
	"encoding/json"
	"net/http"
	"time"

	"go.uber.org/zap"

	"github.com/An-idd/x-beacon/internal/billing"
	"github.com/An-idd/x-beacon/internal/cache"
	"github.com/An-idd/x-beacon/internal/observability"
	"github.com/An-idd/x-beacon/internal/provider"
	"github.com/An-idd/x-beacon/internal/router"
	"github.com/An-idd/x-beacon/internal/server/sse"
	"github.com/An-idd/x-beacon/pkg/tokenizer"
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
	tk *tokenizer.Selector,
	bill *billing.Worker,
	metrics *observability.Metrics,
	exactCache cache.Exact,
	cacheTTL time.Duration,
	cacheKey string,
	semanticCache cache.Semantic,
	req *provider.ChatRequest,
	started time.Time,
	logger *zap.Logger,
	reqID string,
) {
	// streamStats accumulates the data we need at the end of the stream
	// to emit a billing event. Updated as chunks flow.
	var stats streamStats
	stats.provider = ""

	ch, err := rtr.ChatCompletionStream(r.Context(), req)
	if err != nil {
		m := mapProviderError(err)
		logger.Warn("chat stream rejected pre-stream",
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
			Streamed:  true,
			LatencyMs: int(time.Since(started).Milliseconds()),
			ErrorCode: m.Code,
		})
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

	finalStatus := http.StatusOK
	var errCode string
	for ev := range ch {
		if ev.Err != nil {
			m := mapProviderError(ev.Err)
			finalStatus = m.Status
			errCode = m.Code
			emitStreamError(sw, ev.Err, req.Model, reqID, logger)
			break
		}

		stats.observe(ev.Chunk)

		data, err := json.Marshal(ev.Chunk)
		if err != nil {
			logger.Error("encode stream chunk",
				zap.String("req_id", reqID),
				zap.String("model", req.Model),
				zap.Error(err))
			// Same envelope as a mid-stream error: client gets one bad event
			// instead of a silently truncated stream.
			finalStatus = http.StatusInternalServerError
			errCode = "internal_error"
			emitStreamError(sw, err, req.Model, reqID, logger)
			break
		}

		if err := sw.WriteData(data); err != nil {
			// Almost always "client disconnected". ctx will be canceled
			// shortly which exits the producer; nothing more to do here.
			logger.Debug("stream write failed; ending",
				zap.String("req_id", reqID),
				zap.Error(err))
			break
		}
	}

	// Channel closed naturally. If ctx was canceled (client gone), skip the
	// terminal marker — the connection is dead anyway.
	if r.Context().Err() == nil && finalStatus == http.StatusOK {
		_ = sw.WriteData(doneMarker)
	}

	prompt, completion := stats.tokenCounts(tk, req)
	metrics.ObserveRequest(stats.provider, req.Model, finalStatus, time.Since(started).Seconds())
	if finalStatus == http.StatusOK {
		metrics.AddTokens(stats.provider, req.Model, prompt, completion)
	}

	// Cache write-back (Week 10): aggregate the stream into a
	// ChatResponse and Set under the same key the non-stream path
	// uses. shouldCacheResponse enforces the 4-condition gate. We only
	// write on a clean (status=200) end-of-stream — mid-stream errors
	// and client disconnects skip the write.
	if finalStatus == http.StatusOK && r.Context().Err() == nil {
		cacheable := stats.toCachedResponse(req)
		if shouldCacheResponse(cacheable, prompt) {
			if exactCache != nil && cacheKey != "" && cacheTTL > 0 {
				if err := exactCache.Set(r.Context(), cacheKey, cacheable, cacheTTL); err != nil {
					logger.Warn("stream cache write failed",
						zap.String("req_id", reqID),
						zap.String("model", req.Model),
						zap.Error(err))
				} else {
					metrics.IncCacheWrite("exact")
				}
			}
			if semanticCache != nil {
				if err := semanticCache.Insert(r.Context(), req, cacheable); err != nil {
					logger.Warn("stream semantic cache write failed",
						zap.String("req_id", reqID),
						zap.String("model", req.Model),
						zap.Error(err))
				} else {
					metrics.IncCacheWrite("semantic")
				}
			}
		}
	}
	emitBillingEvent(bill, billing.Event{
		StartedAt:        started,
		RequestID:        reqID,
		APIKeyID:         apiKeyIDFrom(r),
		Provider:         stats.provider,
		Model:            req.Model,
		PromptTokens:     prompt,
		CompletionTokens: completion,
		LatencyMs:        int(time.Since(started).Milliseconds()),
		Status:           finalStatus,
		Streamed:         true,
		ErrorCode:        errCode,
	})
}

// streamStats accumulates per-stream observations used at end-of-stream
// to compute the billing event AND, in Week 10, the synthetic
// ChatResponse we write back to the cache. Tracking is best-effort:
// we capture upstream-supplied Usage when present (Anthropic always;
// OpenAI when stream_options.include_usage is set) and otherwise fall
// back to a tokenizer running over the concatenated content.
type streamStats struct {
	provider     string
	id           string // last chunk id seen — providers reuse it across chunks
	created      int64
	model        string
	contentBuf   string // accumulated assistant content for tokenizer + cache write
	finishReason string // captured from the terminal chunk; gates cache write
	promptUsage  int
	outputUsage  int
}

func (s *streamStats) observe(chunk *provider.ChatStreamChunk) {
	if chunk == nil {
		return
	}
	if chunk.ID != "" {
		s.id = chunk.ID
	}
	if chunk.Created != 0 {
		s.created = chunk.Created
	}
	if chunk.Model != "" {
		s.model = chunk.Model
	}
	if chunk.Usage != nil {
		// Provider sent usage. Last-write-wins; some providers send a
		// running tally, others only at the terminal chunk.
		if chunk.Usage.PromptTokens > 0 {
			s.promptUsage = chunk.Usage.PromptTokens
		}
		if chunk.Usage.CompletionTokens > 0 {
			s.outputUsage = chunk.Usage.CompletionTokens
		}
	}
	for _, ch := range chunk.Choices {
		if ch.Delta.Content != "" {
			s.contentBuf += ch.Delta.Content
		}
		if ch.FinishReason != "" {
			s.finishReason = ch.FinishReason
		}
	}
}

// toCachedResponse synthesizes a ChatResponse equivalent to what a
// non-stream call would have returned. Used by the stream cache
// write-back so the same key serves stream and non-stream consumers.
//
// Uses cached/observed model + id when present, falling back to
// per-request defaults so a sparse upstream (Anthropic only sends id
// once) still produces a valid response.
func (s *streamStats) toCachedResponse(req *provider.ChatRequest) *provider.ChatResponse {
	model := s.model
	if model == "" {
		model = req.Model
	}
	id := s.id
	if id == "" {
		id = "chatcmpl-stream"
	}
	created := s.created
	if created == 0 {
		created = time.Now().Unix()
	}
	resp := &provider.ChatResponse{
		ID:      id,
		Object:  "chat.completion",
		Created: created,
		Model:   model,
		Provider: s.provider,
		Choices: []provider.Choice{{
			Index:        0,
			Message:      provider.Message{Role: "assistant", Content: s.contentBuf},
			FinishReason: s.finishReason,
		}},
	}
	if s.promptUsage > 0 || s.outputUsage > 0 {
		resp.Usage = &provider.Usage{
			PromptTokens:     s.promptUsage,
			CompletionTokens: s.outputUsage,
			TotalTokens:      s.promptUsage + s.outputUsage,
		}
	}
	return resp
}

// tokenCounts returns (prompt, completion) for the stream's billing
// event. Prefers usage observed mid-stream; falls back to tokenizer
// estimates over the prompt and the accumulated assistant content.
func (s *streamStats) tokenCounts(tk *tokenizer.Selector, req *provider.ChatRequest) (int, int) {
	prompt, completion := s.promptUsage, s.outputUsage
	if tk == nil {
		return prompt, completion
	}
	t := tk.For(req.Model)
	if prompt == 0 {
		prompt = t.CountMessages(req.Messages)
	}
	if completion == 0 {
		completion = t.CountText(s.contentBuf)
	}
	return prompt, completion
}

// doneMarker is the OpenAI-canonical end-of-stream sentinel. Written as
// raw bytes (no JSON escaping) because clients match the literal string.
var doneMarker = []byte("[DONE]")

// replayChunkRunes is the size of each synthetic stream chunk during
// cache replay. Counted in runes (not bytes) so we never split a UTF-8
// codepoint in half — splitting mid-codepoint produces invalid JSON
// after marshaling and confuses lenient parsers.
//
// 32 runes is small enough that the client sees several frames even on
// short answers (so SSE-aware UIs visibly "stream") but big enough that
// per-chunk JSON overhead doesn't dominate. Not exposed as config —
// chunk granularity is invisible to a correct client and we don't want
// it to become a tuning knob.
const replayChunkRunes = 32

// replayCachedStream emits a previously-cached ChatResponse as if it
// were arriving fresh from the upstream. Same wire shape as a real
// stream: a role-only opener, N content frames, a finish_reason-only
// closer, then [DONE].
//
// No artificial delay between frames. Cache hits are already instant
// from the client's perspective, and adding delay just to "look like"
// streaming would burn server-side resources for no user benefit.
//
// Errors writing to the SSE writer typically mean the client
// disconnected; we stop quietly and return.
func replayCachedStream(
	sw *sse.Writer,
	cached *provider.ChatResponse,
	reqID string,
	logger *zap.Logger,
) {
	if cached == nil {
		return
	}

	// Fabricate a stable id: matches what real providers do (same id
	// across all chunks of one response). Using the cached response's
	// id makes the replay byte-identical to what a non-streamed cache
	// hit returns, which simplifies client-side correlation.
	id := cached.ID
	if id == "" {
		id = "chatcmpl-cache-" + reqID
	}
	created := cached.Created
	if created == 0 {
		created = time.Now().Unix()
	}

	// Frame 1: role-only opener for each choice. OpenAI clients expect
	// to see the role before any content.
	roleChunk := provider.ChatStreamChunk{
		ID: id, Object: "chat.completion.chunk", Created: created, Model: cached.Model,
		Choices: make([]provider.StreamChoice, 0, len(cached.Choices)),
	}
	for _, ch := range cached.Choices {
		roleChunk.Choices = append(roleChunk.Choices, provider.StreamChoice{
			Index: ch.Index,
			Delta: provider.MessageDelta{Role: "assistant"},
		})
	}
	if !replayWriteChunk(sw, &roleChunk, reqID, logger) {
		return
	}

	// Frames 2..N-1: content slices. Emit per-choice content runs in
	// sequence so a multi-choice cached response replays cleanly. In
	// practice n=1 dominates, but we don't assume it.
	for _, ch := range cached.Choices {
		runes := []rune(ch.Message.Content)
		for offset := 0; offset < len(runes); offset += replayChunkRunes {
			end := offset + replayChunkRunes
			if end > len(runes) {
				end = len(runes)
			}
			contentChunk := provider.ChatStreamChunk{
				ID: id, Object: "chat.completion.chunk", Created: created, Model: cached.Model,
				Choices: []provider.StreamChoice{{
					Index: ch.Index,
					Delta: provider.MessageDelta{Content: string(runes[offset:end])},
				}},
			}
			if !replayWriteChunk(sw, &contentChunk, reqID, logger) {
				return
			}
		}
	}

	// Frame N: finish_reason for each choice, empty delta. Some clients
	// gate on this frame to flip "streaming → done" UI state.
	finishChunk := provider.ChatStreamChunk{
		ID: id, Object: "chat.completion.chunk", Created: created, Model: cached.Model,
		Choices: make([]provider.StreamChoice, 0, len(cached.Choices)),
	}
	for _, ch := range cached.Choices {
		fr := ch.FinishReason
		if fr == "" {
			fr = "stop"
		}
		finishChunk.Choices = append(finishChunk.Choices, provider.StreamChoice{
			Index:        ch.Index,
			FinishReason: fr,
		})
	}
	// Replay the cached usage on the terminal chunk so cost / token
	// dashboards see consistent values whether the response came from
	// the upstream or the cache.
	if cached.Usage != nil {
		usageCopy := *cached.Usage
		finishChunk.Usage = &usageCopy
	}
	if !replayWriteChunk(sw, &finishChunk, reqID, logger) {
		return
	}

	// Final [DONE] sentinel.
	if err := sw.WriteData(doneMarker); err != nil {
		logger.Debug("replay [DONE] write failed; ending",
			zap.String("req_id", reqID),
			zap.Error(err))
	}
}

// replayWriteChunk marshals + writes one chunk. Returns false when the
// caller should stop (write error → assume client disconnected).
func replayWriteChunk(
	sw *sse.Writer,
	chunk *provider.ChatStreamChunk,
	reqID string,
	logger *zap.Logger,
) bool {
	data, err := json.Marshal(chunk)
	if err != nil {
		// MarshalJSON only fails on cycles / unsupported types; the
		// chunk shape is plain structs + strings so this is unreachable
		// in practice. Log and stop replay rather than crash.
		logger.Error("replay marshal failed",
			zap.String("req_id", reqID),
			zap.Error(err))
		return false
	}
	if err := sw.WriteData(data); err != nil {
		logger.Debug("replay chunk write failed; ending",
			zap.String("req_id", reqID),
			zap.Error(err))
		return false
	}
	return true
}

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
