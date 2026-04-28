// mockupstream is a tiny OpenAI-compatible upstream used by scripts/bench.sh
// to isolate gateway-only overhead. Returns a canned 200 with usage={1,1,2}
// instantly so the gateway's own latency is what shows up in the histogram.
//
// Listens on $MOCK_ADDR (default 127.0.0.1:9091).
package main

import (
	"encoding/json"
	"log"
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
