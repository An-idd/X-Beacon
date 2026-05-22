// mockupstream is a tiny OpenAI-compatible upstream used by both
// scripts/bench.sh (latency isolation) and the Phase 5 Python SDK compat
// suite (request-shape exercise). It serves a canned 200 in the simple
// case; when the request body carries features the Python SDK suite
// exercises — tools, response_format, logprobs, stream — it adapts the
// response so the SDK sees a properly-shaped reply.
//
// Listens on $MOCK_ADDR (default 127.0.0.1:9091). No state, no auth, no
// control plane: behavior is a pure function of the request body, so
// tests can run in any order and parallelize freely.
package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"encoding/json"
	"io"
	"log"
	"math"
	"net/http"
	"os"
	"time"
)

func main() {
	addr := os.Getenv("MOCK_ADDR")
	if addr == "" {
		addr = "127.0.0.1:9091"
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/v1/chat/completions", chatCompletions)
	mux.HandleFunc("/v1/models", modelsList)
	mux.HandleFunc("/v1/embeddings", embeddings)

	srv := &http.Server{
		Addr:              addr,
		Handler:           mux,
		ReadHeaderTimeout: 5 * time.Second,
	}
	log.Printf("mockupstream: listening on %s", addr)
	if err := srv.ListenAndServe(); err != nil {
		log.Fatal(err)
	}
}

// chatRequest captures only the fields whose presence changes the canned
// reply. Everything else (messages, model name, etc.) is ignored — the
// mock is here to validate wire shape, not to simulate a real LLM.
type chatRequest struct {
	Stream         bool            `json:"stream"`
	Tools          json.RawMessage `json:"tools"`
	Logprobs       *bool           `json:"logprobs"`
	ResponseFormat *struct {
		Type string `json:"type"`
	} `json:"response_format"`
	Model string `json:"model"`
}

func chatCompletions(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	defer r.Body.Close()
	// Restore for downstream code that wants to re-read; harmless if
	// nobody does.
	r.Body = io.NopCloser(bytes.NewReader(body))

	var req chatRequest
	_ = json.Unmarshal(body, &req)

	if req.Stream {
		writeStreamingChat(w, req.Model)
		return
	}

	switch {
	case len(req.Tools) > 0:
		writeToolCallChat(w, req.Model)
	case req.ResponseFormat != nil && req.ResponseFormat.Type == "json_object":
		writeJSONModeChat(w, req.Model)
	case req.Logprobs != nil && *req.Logprobs:
		writeLogprobsChat(w, req.Model)
	default:
		writePlainChat(w, req.Model)
	}
}

func modelName(fallback string) string {
	if fallback != "" {
		return fallback
	}
	return "gpt-4o-mini"
}

// writePlainChat is the historic canned reply preserved for bench.sh and
// any non-feature-flag test. Identical bytes to the pre-Phase-5 version
// (modulo `model` echoing the request) so existing fixtures don't drift.
func writePlainChat(w http.ResponseWriter, model string) {
	resp := map[string]any{
		"id":      "chatcmpl-mock",
		"object":  "chat.completion",
		"created": time.Now().Unix(),
		"model":   modelName(model),
		"choices": []map[string]any{{
			"index":         0,
			"message":       map[string]any{"role": "assistant", "content": "ok"},
			"finish_reason": "stop",
		}},
		"usage": map[string]any{
			"prompt_tokens":     1,
			"completion_tokens": 1,
			"total_tokens":      2,
		},
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(resp)
}

// writeToolCallChat emits an assistant message with a single tool_call.
// `arguments` is a JSON STRING (not an object) to match the OpenAI wire
// contract — this is the field that has historically broken gateways
// that helpfully "parse and re-encode" tool args.
func writeToolCallChat(w http.ResponseWriter, model string) {
	resp := map[string]any{
		"id":      "chatcmpl-mock-tools",
		"object":  "chat.completion",
		"created": time.Now().Unix(),
		"model":   modelName(model),
		"choices": []map[string]any{{
			"index": 0,
			"message": map[string]any{
				"role":    "assistant",
				"content": nil,
				"tool_calls": []map[string]any{{
					"id":   "call_mock_1",
					"type": "function",
					"function": map[string]any{
						"name":      "search",
						"arguments": `{"q":"hello"}`,
					},
				}},
			},
			"finish_reason": "tool_calls",
		}},
		"usage": map[string]any{
			"prompt_tokens":     8,
			"completion_tokens": 12,
			"total_tokens":      20,
		},
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(resp)
}

// writeJSONModeChat returns a parseable JSON-object string as content,
// matching the constraint OpenAI imposes when response_format.type is
// "json_object".
func writeJSONModeChat(w http.ResponseWriter, model string) {
	resp := map[string]any{
		"id":      "chatcmpl-mock-json",
		"object":  "chat.completion",
		"created": time.Now().Unix(),
		"model":   modelName(model),
		"choices": []map[string]any{{
			"index": 0,
			"message": map[string]any{
				"role":    "assistant",
				"content": `{"answer":"42","unit":"the meaning of life"}`,
			},
			"finish_reason": "stop",
		}},
		"usage": map[string]any{"prompt_tokens": 5, "completion_tokens": 14, "total_tokens": 19},
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(resp)
}

// writeLogprobsChat returns a minimal but well-formed logprobs payload.
// The SDK is strict about shape: `choices[].logprobs.content` is an
// array of {token, logprob, bytes, top_logprobs} entries.
func writeLogprobsChat(w http.ResponseWriter, model string) {
	resp := map[string]any{
		"id":      "chatcmpl-mock-lp",
		"object":  "chat.completion",
		"created": time.Now().Unix(),
		"model":   modelName(model),
		"choices": []map[string]any{{
			"index":         0,
			"message":       map[string]any{"role": "assistant", "content": "ok"},
			"finish_reason": "stop",
			"logprobs": map[string]any{
				"content": []map[string]any{
					{"token": "ok", "logprob": -0.1, "bytes": []int{111, 107}, "top_logprobs": []any{}},
				},
			},
		}},
		"usage": map[string]any{"prompt_tokens": 1, "completion_tokens": 1, "total_tokens": 2},
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(resp)
}

// writeStreamingChat emits a deterministic 3-chunk SSE stream + [DONE].
// Frame layout mirrors what OpenAI's API actually sends, including the
// trailing `data: [DONE]\n\n` terminator the SDK looks for.
func writeStreamingChat(w http.ResponseWriter, model string) {
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.WriteHeader(http.StatusOK)
	fl, _ := w.(http.Flusher)

	created := time.Now().Unix()
	id := "chatcmpl-mock-stream"
	m := modelName(model)
	chunks := []map[string]any{
		{"id": id, "object": "chat.completion.chunk", "created": created, "model": m,
			"choices": []map[string]any{{"index": 0, "delta": map[string]any{"role": "assistant"}}}},
		{"id": id, "object": "chat.completion.chunk", "created": created, "model": m,
			"choices": []map[string]any{{"index": 0, "delta": map[string]any{"content": "hello "}}}},
		{"id": id, "object": "chat.completion.chunk", "created": created, "model": m,
			"choices": []map[string]any{{"index": 0, "delta": map[string]any{"content": "world"}}}},
		{"id": id, "object": "chat.completion.chunk", "created": created, "model": m,
			"choices": []map[string]any{{"index": 0, "delta": map[string]any{}, "finish_reason": "stop"}}},
	}
	for _, c := range chunks {
		raw, _ := json.Marshal(c)
		_, _ = io.WriteString(w, "data: ")
		_, _ = w.Write(raw)
		_, _ = io.WriteString(w, "\n\n")
		if fl != nil {
			fl.Flush()
		}
	}
	_, _ = io.WriteString(w, "data: [DONE]\n\n")
	if fl != nil {
		fl.Flush()
	}
}

// embeddings returns deterministic 1536-dim float32 vectors derived
// from sha256 of the input text. Same text → same vector. Used by
// the Week 10 E2E smoke so the gateway's semantic cache can be
// exercised without an OpenAI account.
func embeddings(w http.ResponseWriter, r *http.Request) {
	defer r.Body.Close()
	body, _ := io.ReadAll(r.Body)
	var req struct {
		Input []string `json:"input"`
		Model string   `json:"model"`
	}
	_ = json.Unmarshal(body, &req)

	dim := 1536
	data := make([]map[string]any, len(req.Input))
	for i, text := range req.Input {
		data[i] = map[string]any{
			"index":     i,
			"object":    "embedding",
			"embedding": deterministicVec(text, dim),
		}
	}
	resp := map[string]any{
		"object": "list",
		"data":   data,
		"model":  req.Model,
		"usage": map[string]any{
			"prompt_tokens": len(req.Input),
			"total_tokens":  len(req.Input),
		},
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(resp)
}

// deterministicVec produces a stable 1536-dim float32 vector from the
// input text. The first sha256(text) seeds successive blocks; values
// are mapped to [-1, 1]. Identical inputs → bit-identical vectors,
// which is the property the semantic cache E2E smoke relies on.
func deterministicVec(text string, dim int) []float32 {
	out := make([]float32, dim)
	seed := sha256.Sum256([]byte(text))
	buf := seed[:]
	idx := 0
	for idx < dim {
		// Hash the previous 32 bytes to extend the stream.
		next := sha256.Sum256(buf)
		buf = next[:]
		for j := 0; j+4 <= len(buf) && idx < dim; j += 4 {
			u := binary.LittleEndian.Uint32(buf[j : j+4])
			f := float32(u)/float32(math.MaxUint32)*2 - 1
			out[idx] = f
			idx++
		}
	}
	return out
}

func modelsList(w http.ResponseWriter, r *http.Request) {
	resp := map[string]any{
		"object": "list",
		"data": []map[string]any{
			{"id": "gpt-4o-mini", "object": "model", "owned_by": "mock"},
		},
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(resp)
}
