package server

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"time"

	"go.uber.org/zap"

	"github.com/An-idd/x-beacon/internal/billing"
	"github.com/An-idd/x-beacon/internal/observability"
	"github.com/An-idd/x-beacon/internal/provider/registry"
	"github.com/An-idd/x-beacon/internal/server/middleware"
)

// messagesForwarder is the narrow slice of the anthropic adapter the
// native /v1/messages endpoint needs. Satisfied by *anthropic.Provider;
// providers of other types don't implement it, which is exactly the
// routing signal: a model served by a non-Anthropic provider cannot be
// reached through the native protocol.
type messagesForwarder interface {
	Name() string
	ForwardMessages(ctx context.Context, body []byte, betaHeader string) (*http.Response, error)
}

// messagesHandler serves the Anthropic-native /v1/messages endpoint as a
// verbatim passthrough: the request body is forwarded byte-identical to
// the upstream and the response (JSON or SSE) is piped back untouched.
// Not parsing is the point — thinking blocks, prompt caching, and future
// protocol features survive because the gateway never re-marshals them.
//
// Auth + rate-limit middleware run in front (shared /v1 group). Usage is
// scanned out-of-band for billing/metrics without modifying the stream.
//
// ponytail: no retry/failover/breaker on this path — the router layer
// models OpenAI-format requests only. Add a raw-body retry wrapper if
// native-endpoint reliability ever needs to match /v1/chat/completions.
func messagesHandler(reg *registry.Registry, bill *billing.Worker, metrics *observability.Metrics, logger *zap.Logger) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		started := time.Now()
		reqID := middleware.RequestIDFrom(r.Context())

		body, err := io.ReadAll(http.MaxBytesReader(nil, r.Body, maxRequestBytes))
		if err != nil {
			var maxErr *http.MaxBytesError
			if errors.As(err, &maxErr) {
				writeAnthropicError(w, http.StatusRequestEntityTooLarge,
					"invalid_request_error", "Request body too large")
				return
			}
			writeAnthropicError(w, http.StatusBadRequest,
				"invalid_request_error", "Failed to read request body")
			return
		}

		// Minimal peek: model (routing) + stream (response handling).
		// The body itself is forwarded untouched.
		var peek struct {
			Model  string `json:"model"`
			Stream bool   `json:"stream"`
		}
		if err := json.Unmarshal(body, &peek); err != nil || peek.Model == "" {
			writeAnthropicError(w, http.StatusBadRequest,
				"invalid_request_error", "Request body must be valid JSON with a model field")
			return
		}

		prov, err := reg.ResolveModel(peek.Model)
		if err != nil {
			writeAnthropicError(w, http.StatusNotFound,
				"not_found_error", "model: "+peek.Model)
			return
		}
		fwd, ok := prov.(messagesForwarder)
		if !ok {
			// Model exists but is served by a non-Anthropic provider;
			// the native protocol can't reach it. 404 mirrors what
			// Anthropic returns for unknown models.
			writeAnthropicError(w, http.StatusNotFound,
				"not_found_error", "model "+peek.Model+" is not served by an Anthropic-protocol provider")
			return
		}

		resp, err := fwd.ForwardMessages(r.Context(), body, r.Header.Get("anthropic-beta"))
		if err != nil {
			logger.Warn("messages forward failed",
				zap.String("req_id", reqID),
				zap.String("model", peek.Model),
				zap.Error(err))
			metrics.ObserveRequest(fwd.Name(), peek.Model, http.StatusBadGateway, time.Since(started).Seconds())
			writeAnthropicError(w, http.StatusBadGateway,
				"api_error", "Upstream request failed")
			return
		}
		defer resp.Body.Close()

		// Pass through the headers SDKs act on.
		for _, h := range []string{"Content-Type", "Retry-After", "Request-Id", "Anthropic-Ratelimit-Requests-Remaining", "Anthropic-Ratelimit-Tokens-Remaining"} {
			if v := resp.Header.Get(h); v != "" {
				w.Header().Set(h, v)
			}
		}

		var usage anthropicUsage
		streamed := strings.HasPrefix(resp.Header.Get("Content-Type"), "text/event-stream")
		if streamed {
			w.WriteHeader(resp.StatusCode)
			usage = pipeAnthropicSSE(w, resp.Body)
		} else {
			respBody, readErr := io.ReadAll(resp.Body)
			if readErr != nil {
				logger.Warn("messages upstream read failed",
					zap.String("req_id", reqID), zap.Error(readErr))
				metrics.ObserveRequest(fwd.Name(), peek.Model, http.StatusBadGateway, time.Since(started).Seconds())
				writeAnthropicError(w, http.StatusBadGateway, "api_error", "Upstream read failed")
				return
			}
			w.WriteHeader(resp.StatusCode)
			_, _ = w.Write(respBody)
			if resp.StatusCode == http.StatusOK {
				var envelope struct {
					Usage anthropicUsage `json:"usage"`
				}
				_ = json.Unmarshal(respBody, &envelope) // best-effort; zero usage on parse failure
				usage = envelope.Usage
			}
		}

		metrics.ObserveRequest(fwd.Name(), peek.Model, resp.StatusCode, time.Since(started).Seconds())
		if resp.StatusCode == http.StatusOK {
			metrics.AddTokens(fwd.Name(), peek.Model, usage.InputTokens, usage.OutputTokens)
			emitBillingEvent(bill, billing.Event{
				StartedAt:        started,
				RequestID:        reqID,
				APIKeyID:         apiKeyIDFrom(r),
				Provider:         fwd.Name(),
				Model:            peek.Model,
				PromptTokens:     usage.InputTokens,
				CompletionTokens: usage.OutputTokens,
				LatencyMs:        int(time.Since(started).Milliseconds()),
				Status:           http.StatusOK,
				Streamed:         streamed,
			})
		}
	}
}

// anthropicUsage is the subset of Anthropic's usage block the gateway
// accounts for. Cache-specific counters pass through in the body but are
// not modeled here.
type anthropicUsage struct {
	InputTokens  int `json:"input_tokens"`
	OutputTokens int `json:"output_tokens"`
}

// pipeAnthropicSSE copies the upstream SSE stream to the client verbatim,
// flushing per line, while scanning `data:` payloads for usage counters
// (message_start carries input_tokens; message_delta carries the final
// output_tokens). The scan never mutates the bytes written to the client.
func pipeAnthropicSSE(w http.ResponseWriter, body io.Reader) anthropicUsage {
	var usage anthropicUsage
	flusher, _ := w.(http.Flusher)
	reader := bufio.NewReader(body)
	for {
		line, err := reader.ReadBytes('\n')
		if len(line) > 0 {
			if _, werr := w.Write(line); werr != nil {
				break // client gone; upstream ctx cancellation follows
			}
			if flusher != nil {
				flusher.Flush()
			}
			scanUsageLine(line, &usage)
		}
		if err != nil {
			break // io.EOF or upstream error — either way the stream is over
		}
	}
	return usage
}

// scanUsageLine extracts usage counters from one SSE line. Cheap
// substring gate first so non-usage lines cost one bytes.Contains.
func scanUsageLine(line []byte, usage *anthropicUsage) {
	payload, ok := bytes.CutPrefix(line, []byte("data: "))
	if !ok || !bytes.Contains(payload, []byte(`"usage"`)) {
		return
	}
	var ev struct {
		Type    string `json:"type"`
		Message struct {
			Usage anthropicUsage `json:"usage"`
		} `json:"message"`
		Usage anthropicUsage `json:"usage"`
	}
	if json.Unmarshal(payload, &ev) != nil {
		return
	}
	switch ev.Type {
	case "message_start":
		usage.InputTokens = ev.Message.Usage.InputTokens
	case "message_delta":
		usage.OutputTokens = ev.Usage.OutputTokens
	}
}

// writeAnthropicError emits an Anthropic-shaped error envelope:
// {"type":"error","error":{"type":...,"message":...}}. The native
// endpoint must never answer in the OpenAI error shape — Anthropic SDKs
// key their error classes off this structure.
func writeAnthropicError(w http.ResponseWriter, status int, errType, message string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]any{
		"type": "error",
		"error": map[string]string{
			"type":    errType,
			"message": message,
		},
	})
}
