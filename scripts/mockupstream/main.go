// mockupstream is a tiny OpenAI-compatible upstream used by scripts/bench.sh
// to isolate gateway-only overhead. Returns a canned 200 with usage={1,1,2}
// instantly so the gateway's own latency is what shows up in the histogram.
//
// Listens on $MOCK_ADDR (default 127.0.0.1:9091).
package main

import (
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

func chatCompletions(w http.ResponseWriter, r *http.Request) {
	resp := map[string]any{
		"id":      "chatcmpl-mock",
		"object":  "chat.completion",
		"created": time.Now().Unix(),
		"model":   "gpt-4o-mini",
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
