package server

import (
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"time"

	"go.uber.org/zap"

	"github.com/An-idd/x-beacon/internal/audit"
	"github.com/An-idd/x-beacon/internal/server/middleware"
)

// adminAuditHandler exposes the audit query: actor / action /
// target_type filters + RFC3339 range. Mirrors /admin/logs in shape
// so the WebUI can reuse its filter idioms.
//
// 31d window cap (vs 7d for /admin/logs) — audit volume is so low
// (operator clicks per day) that a month is fine; common admin
// query is "what happened this month".
func adminAuditHandler(recorder audit.Recorder, logger *zap.Logger) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		reqID := middleware.RequestIDFrom(r.Context())
		q := r.URL.Query()

		opts, mapped, ok := parseAuditQuery(q)
		if !ok {
			writeError(w, mapped, reqID)
			return
		}

		rows, total, err := recorder.Query(r.Context(), opts)
		if err != nil {
			switch {
			case errors.Is(err, audit.ErrWindowInvalid):
				writeError(w, mappedError{
					Status: http.StatusBadRequest, Type: "invalid_request_error",
					Code: "invalid_window", Message: err.Error(),
				}, reqID)
			case errors.Is(err, audit.ErrWindowTooWide):
				writeError(w, mappedError{
					Status: http.StatusBadRequest, Type: "invalid_request_error",
					Code: "window_too_wide", Message: "Audit window must be <= 31d",
				}, reqID)
			case errors.Is(err, audit.ErrNotConfigured):
				writeError(w, mappedError{
					Status: http.StatusServiceUnavailable, Type: "internal_error",
					Code: "audit_disabled", Message: "Audit log not configured (no DB)",
				}, reqID)
			default:
				logger.Error("admin audit query failed",
					zap.String("req_id", reqID), zap.Error(err))
				writeError(w, mappedError{
					Status: http.StatusInternalServerError, Type: "internal_error",
					Message: "Failed to query audit log",
				}, reqID)
			}
			return
		}

		items := make([]auditDTO, 0, len(rows))
		for _, row := range rows {
			items = append(items, auditDTO{
				ID:         row.ID,
				OccurredAt: row.OccurredAt,
				ActorID:    row.ActorID,
				ActorLabel: row.ActorLabel,
				Action:     row.Action,
				TargetType: row.TargetType,
				TargetID:   row.TargetID,
				Metadata:   row.Metadata,
				RequestID:  row.RequestID,
			})
		}

		nextOffset := opts.Offset + len(items)
		if nextOffset >= total {
			nextOffset = -1
		}

		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"object":      "list",
			"items":       items,
			"total":       total,
			"next_offset": nextOffset,
		})
	}
}

type auditDTO struct {
	ID         int64           `json:"id"`
	OccurredAt time.Time       `json:"occurred_at"`
	ActorID    string          `json:"actor_id"`
	ActorLabel string          `json:"actor_label"`
	Action     string          `json:"action"`
	TargetType string          `json:"target_type"`
	TargetID   string          `json:"target_id"`
	Metadata   json.RawMessage `json:"metadata,omitempty"`
	RequestID  string          `json:"request_id,omitempty"`
}

func parseAuditQuery(q map[string][]string) (audit.QueryOpts, mappedError, bool) {
	get := func(k string) string {
		if v, ok := q[k]; ok && len(v) > 0 {
			return v[0]
		}
		return ""
	}

	startRaw, endRaw := get("start"), get("end")
	if startRaw == "" || endRaw == "" {
		return audit.QueryOpts{}, mappedError{
			Status: http.StatusBadRequest, Type: "invalid_request_error",
			Code: "missing_window", Message: "Both 'start' and 'end' (RFC3339) are required",
		}, false
	}
	start, err := time.Parse(time.RFC3339, startRaw)
	if err != nil {
		return audit.QueryOpts{}, mappedError{
			Status: http.StatusBadRequest, Type: "invalid_request_error",
			Code: "invalid_start", Message: "Field 'start' must be RFC3339",
		}, false
	}
	end, err := time.Parse(time.RFC3339, endRaw)
	if err != nil {
		return audit.QueryOpts{}, mappedError{
			Status: http.StatusBadRequest, Type: "invalid_request_error",
			Code: "invalid_end", Message: "Field 'end' must be RFC3339",
		}, false
	}

	out := audit.QueryOpts{
		Start:      start,
		End:        end,
		ActorID:    get("actor_id"),
		Action:     get("action"),
		TargetType: get("target_type"),
	}
	if v := get("limit"); v != "" {
		n, perr := strconv.Atoi(v)
		if perr != nil || n < 1 {
			return audit.QueryOpts{}, mappedError{
				Status: http.StatusBadRequest, Type: "invalid_request_error",
				Code: "invalid_limit", Message: "Field 'limit' must be a positive integer (max 200)",
			}, false
		}
		out.Limit = n
	}
	if v := get("offset"); v != "" {
		n, perr := strconv.Atoi(v)
		if perr != nil || n < 0 {
			return audit.QueryOpts{}, mappedError{
				Status: http.StatusBadRequest, Type: "invalid_request_error",
				Code: "invalid_offset", Message: "Field 'offset' must be a non-negative integer",
			}, false
		}
		out.Offset = n
	}
	return out, mappedError{}, true
}
