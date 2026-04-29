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
	"github.com/An-idd/x-beacon/internal/cache"
	"github.com/An-idd/x-beacon/internal/observability"
	"github.com/An-idd/x-beacon/internal/provider"
	"github.com/An-idd/x-beacon/internal/router"
	"github.com/An-idd/x-beacon/internal/server/middleware"
	"github.com/An-idd/x-beacon/internal/server/sse"
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
func chatCompletionsHandler(rtr *router.Router, tk *tokenizer.Selector, bill *billing.Worker, metrics *observability.Metrics, exactCache cache.Exact, cacheTTL time.Duration, semanticCache cache.Semantic, logger *zap.Logger) http.HandlerFunc {
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
			// Week 10: streaming requests share keys with non-streaming
			// (cache.Key excludes req.Stream — see cache.Key doc). On a
			// hit we replay the cached response as synthetic SSE
			// frames; on a miss we fall through to the live upstream
			// path AND record the resulting stream back into cache so
			// future requests of either shape can hit. The bypass
			// header is gone — clients see the same hit/miss values
			// as non-stream requests now.
			var streamCacheKey string
			if exactCache != nil {
				if key, err := cache.Key(req); err == nil {
					streamCacheKey = key
					lookupStart := time.Now()
					cached, lookupErr := exactCache.Get(r.Context(), key)
					lookupSec := time.Since(lookupStart).Seconds()
					switch {
					case lookupErr == nil:
						metrics.ObserveCacheLookup("hit", lookupSec)
						w.Header().Set("X-X-Beacon-Cache", "hit")
						sw, sseErr := sse.New(w)
						if sseErr != nil {
							writeError(w, mappedError{
								Status:  http.StatusInternalServerError,
								Type:    "internal_error",
								Message: "Server does not support streaming",
							}, reqID)
							return
						}
						replayCachedStream(sw, cached, reqID, logger)
						metrics.IncCacheHit("exact")
						metrics.ObserveRequest(cached.Provider, req.Model, http.StatusOK, time.Since(started).Seconds())
						return
					case errors.Is(lookupErr, cache.ErrMiss):
						metrics.ObserveCacheLookup("miss", lookupSec)
					default:
						metrics.ObserveCacheLookup("error", lookupSec)
						logger.Warn("stream cache lookup failed; treating as miss",
							zap.String("req_id", reqID),
							zap.String("model", req.Model),
							zap.Error(lookupErr))
					}
				}
				w.Header().Set("X-X-Beacon-Cache", "miss")
			}

			// Stream-path semantic lookup (mirrors non-stream branch).
			// Hit replays as synthetic SSE; miss falls through to
			// upstream where handleChatStream's write-back also fires
			// for the semantic layer.
			if semanticCache != nil {
				semStart := time.Now()
				cached, similarity, semErr := semanticCache.Lookup(r.Context(), req)
				semElapsed := time.Since(semStart).Seconds()
				switch {
				case semErr == nil:
					metrics.ObserveSemanticLookup("hit", semElapsed)
					w.Header().Set("X-X-Beacon-Cache", "hit")
					w.Header().Set("X-X-Beacon-Cache-Layer", "semantic")
					sw, sseErr := sse.New(w)
					if sseErr != nil {
						writeError(w, mappedError{
							Status:  http.StatusInternalServerError,
							Type:    "internal_error",
							Message: "Server does not support streaming",
						}, reqID)
						return
					}
					replayCachedStream(sw, cached, reqID, logger)
					metrics.IncCacheHit("semantic")
					metrics.ObserveSemanticSimilarity(similarity)
					metrics.ObserveRequest(cached.Provider, req.Model, http.StatusOK, time.Since(started).Seconds())
					return
				case errors.Is(semErr, cache.ErrMiss):
					metrics.ObserveSemanticLookup("miss", semElapsed)
					if similarity > 0 {
						metrics.ObserveSemanticSimilarity(similarity)
					}
				default:
					metrics.ObserveSemanticLookup("error", semElapsed)
					logger.Warn("stream semantic lookup failed; treating as miss",
						zap.String("req_id", reqID),
						zap.String("model", req.Model),
						zap.Error(semErr))
				}
			}
			handleChatStream(w, r, rtr, tk, bill, metrics, exactCache, cacheTTL, streamCacheKey, semanticCache, req, started, logger, reqID)
			return
		}

		// Exact-match cache lookup. Best-effort: backend errors and miss
		// fall through to the upstream call. A hit short-circuits the
		// router, billing, and tokenizer paths — the response is byte-
		// for-byte what we previously cached, so re-attributing tokens
		// or recording a billing row would double-count.
		//
		// cacheKey is reused after the upstream call to write the
		// response back; computing the key once keeps the hash work to
		// O(request) rather than O(2 × request).
		var cacheKey string
		if exactCache != nil {
			if key, err := cache.Key(req); err == nil {
				cacheKey = key
				lookupStart := time.Now()
				cached, err := exactCache.Get(r.Context(), key)
				lookupSec := time.Since(lookupStart).Seconds()
				switch {
				case err == nil:
					metrics.ObserveCacheLookup("hit", lookupSec)
					w.Header().Set("X-X-Beacon-Cache", "hit")
					w.Header().Set("Content-Type", "application/json")
					w.WriteHeader(http.StatusOK)
					if encErr := jsonEncode(w, cached); encErr != nil {
						logger.Error("failed to encode cached response",
							zap.String("req_id", reqID),
							zap.Error(encErr))
					}
					metrics.IncCacheHit("exact")
					metrics.ObserveRequest(cached.Provider, req.Model, http.StatusOK, time.Since(started).Seconds())
					return
				case errors.Is(err, cache.ErrMiss):
					metrics.ObserveCacheLookup("miss", lookupSec)
					// Expected on cold keys — fall through to upstream.
				default:
					metrics.ObserveCacheLookup("error", lookupSec)
					// Backend / decode error: log once and keep going.
					// Cached corruption is auto-healed on the next Set.
					logger.Warn("cache lookup failed; treating as miss",
						zap.String("req_id", reqID),
						zap.String("model", req.Model),
						zap.Error(err))
				}
			}
			w.Header().Set("X-X-Beacon-Cache", "miss")
		}

		// Semantic cache lookup (Week 10): exact missed; try the
		// similarity layer before paying the upstream call. Decision
		// 6 / Option B: hits stay in semantic only — we do NOT promote
		// them into exact (avoids cache amplification + lets threshold
		// tuning take effect cleanly).
		if semanticCache != nil {
			semStart := time.Now()
			cached, similarity, semErr := semanticCache.Lookup(r.Context(), req)
			semElapsed := time.Since(semStart).Seconds()
			switch {
			case semErr == nil:
				metrics.ObserveSemanticLookup("hit", semElapsed)
				w.Header().Set("X-X-Beacon-Cache", "hit")
				w.Header().Set("X-X-Beacon-Cache-Layer", "semantic")
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusOK)
				if encErr := jsonEncode(w, cached); encErr != nil {
					logger.Error("failed to encode semantic cached response",
						zap.String("req_id", reqID),
						zap.Error(encErr))
				}
				metrics.IncCacheHit("semantic")
				metrics.ObserveSemanticSimilarity(similarity)
				metrics.ObserveRequest(cached.Provider, req.Model, http.StatusOK, time.Since(started).Seconds())
				return
			case errors.Is(semErr, cache.ErrMiss):
				metrics.ObserveSemanticLookup("miss", semElapsed)
				// Below threshold or empty index — log similarity for
				// near-miss tuning even though the request will go to
				// the upstream.
				if similarity > 0 {
					metrics.ObserveSemanticSimilarity(similarity)
				}
			default:
				metrics.ObserveSemanticLookup("error", semElapsed)
				logger.Warn("semantic cache lookup failed; treating as miss",
					zap.String("req_id", reqID),
					zap.String("model", req.Model),
					zap.Error(semErr))
			}
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

		// Write-back to cache (Decision 3 anti-pollution gate). All
		// four guards must hold: 200 (implicit — error branch already
		// returned), finish_reason=stop, non-empty content, prompt
		// tokens > 0. Set is best-effort; failures are logged but do
		// not affect the client (response already written).
		if cacheKey != "" && cacheTTL > 0 && shouldCacheResponse(resp, promptTok) {
			if err := exactCache.Set(r.Context(), cacheKey, resp, cacheTTL); err != nil {
				logger.Warn("cache write failed",
					zap.String("req_id", reqID),
					zap.String("model", req.Model),
					zap.Error(err))
			} else {
				metrics.IncCacheWrite("exact")
			}
		}

		// Semantic write-back (Week 10): only on responses that pass
		// the same anti-pollution gate. Embed cost (~50-200ms) is paid
		// after the response has already been flushed to the client,
		// so it doesn't affect TTFB; it does keep the request goroutine
		// alive longer, which the worker pool absorbs naturally.
		if semanticCache != nil && shouldCacheResponse(resp, promptTok) {
			if err := semanticCache.Insert(r.Context(), req, resp); err != nil {
				logger.Warn("semantic cache write failed",
					zap.String("req_id", reqID),
					zap.String("model", req.Model),
					zap.Error(err))
			} else {
				metrics.IncCacheWrite("semantic")
			}
		}

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

// shouldCacheResponse enforces the Week 9 anti-pollution rules
// (Decision 3): the response must be a fully-formed answer that a
// future identical request can safely receive verbatim.
//
//   - finish_reason == "stop": rules out length-truncated, content-
//     filtered, and tool-call branches; clients that hit these
//     typically retry with different params, so caching them traps
//     the next caller into the same dead end.
//   - non-empty content in at least one choice: defends against the
//     rare upstream that returns a 200 with empty assistant content.
//   - promptTok > 0: a sanity check that the upstream returned (or
//     we computed) plausible token counts; zero usually means the
//     usage block was malformed and the response is suspect.
//
// 200-status is implicit — the error branch already returned before
// reaching this gate.
func shouldCacheResponse(resp *provider.ChatResponse, promptTok int) bool {
	if resp == nil || promptTok <= 0 || len(resp.Choices) == 0 {
		return false
	}
	first := resp.Choices[0]
	if first.FinishReason != "stop" {
		return false
	}
	for _, ch := range resp.Choices {
		if ch.Message.Content != "" {
			return true
		}
	}
	return false
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
