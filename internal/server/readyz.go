package server

import (
	"context"
	"encoding/json"
	"net/http"
	"time"
)

// readinessTimeout caps the total /readyz probe budget. A single slow
// dependency must not be able to make the probe time out at the LB
// layer (typical k8s readiness timeout is 1-2s). All checkers share
// this deadline.
const readinessTimeout = 1 * time.Second

// ReadinessChecker is one slice of "is dependency X reachable" plumbed
// in by main. Name appears in the response body so operators can tell at
// a glance which dep is down.
type ReadinessChecker struct {
	Name  string
	Check func(ctx context.Context) error
}

// readyzResponse is the JSON body returned by /readyz. It deliberately
// surfaces per-check error strings — those have already been sanitized
// by the underlying clients (pgx, go-redis) so they don't echo DSNs or
// secrets, and operators get an immediate cause rather than "unready".
type readyzResponse struct {
	Ready  bool                       `json:"ready"`
	Checks map[string]readyzCheckBody `json:"checks"`
}

type readyzCheckBody struct {
	OK    bool   `json:"ok"`
	Error string `json:"error,omitempty"`
}

// readyzHandler returns a handler that runs every checker under a shared
// timeout and writes a 200 only when all are OK. Empty checker slice
// returns 200 (the gateway has nothing to verify; the caller is asking
// "are you up?", and the answer is yes).
func readyzHandler(checkers []ReadinessChecker) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		ctx, cancel := context.WithTimeout(r.Context(), readinessTimeout)
		defer cancel()

		results := make(map[string]readyzCheckBody, len(checkers))
		allOK := true
		for _, c := range checkers {
			err := c.Check(ctx)
			body := readyzCheckBody{OK: err == nil}
			if err != nil {
				body.Error = err.Error()
				allOK = false
			}
			results[c.Name] = body
		}

		status := http.StatusOK
		if !allOK {
			status = http.StatusServiceUnavailable
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_ = json.NewEncoder(w).Encode(readyzResponse{Ready: allOK, Checks: results})
	}
}
