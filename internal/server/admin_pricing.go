package server

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"sort"
	"time"

	"github.com/go-chi/chi/v5"
	"go.uber.org/zap"

	"github.com/An-idd/x-beacon/internal/audit"
	"github.com/An-idd/x-beacon/internal/billing"
	"github.com/An-idd/x-beacon/internal/server/middleware"
)

// adminPricingHandlers builds the four /admin/pricing routes. The
// PricingCache is shared with the chat hot path; writes go through the
// cache's Set/Delete which both persists to model_pricing and refreshes
// the in-memory snapshot atomically. Single-instance deployments are
// fully synchronous; multi-instance ones get bottom-line consistency
// from the periodic reload (default 30 min).
func adminPricingHandlers(cache *billing.PricingCache, recorder audit.Recorder, logger *zap.Logger) chi.Router {
	r := chi.NewRouter()
	r.Get("/", listPricing(cache))
	r.Get("/{model}", getPricing(cache))
	r.Put("/{model}", upsertPricing(cache, recorder, logger))
	r.Delete("/{model}", deletePricing(cache, recorder, logger))
	return r
}

// pricingDTO is the on-the-wire shape. Floats are exposed (per_1k) so
// admins don't have to multiply by 1_000_000 by hand. Conversion to/from
// micro-units happens at this boundary; storage is always integer.
type pricingDTO struct {
	Model            string    `json:"model"`
	Currency         string    `json:"currency"`
	InputPer1k       float64   `json:"input_per_1k"`
	OutputPer1k      float64   `json:"output_per_1k"`
	InputPer1kMicro  int64     `json:"input_per_1k_micro"`
	OutputPer1kMicro int64     `json:"output_per_1k_micro"`
	UpdatedAt        time.Time `json:"updated_at,omitempty"`
}

func dtoFromRate(r billing.Rate) pricingDTO {
	return pricingDTO{
		Model:            r.Model,
		Currency:         r.Currency,
		InputPer1k:       float64(r.InputPer1kMicro) / 1_000_000,
		OutputPer1k:      float64(r.OutputPer1kMicro) / 1_000_000,
		InputPer1kMicro:  r.InputPer1kMicro,
		OutputPer1kMicro: r.OutputPer1kMicro,
		UpdatedAt:        r.UpdatedAt,
	}
}

func listPricing(cache *billing.PricingCache) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		all := cache.All()
		sort.Slice(all, func(i, j int) bool { return all[i].Model < all[j].Model })

		out := make([]pricingDTO, len(all))
		for i, rate := range all {
			out[i] = dtoFromRate(rate)
		}

		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"object": "list",
			"data":   out,
		})
	}
}

func getPricing(cache *billing.PricingCache) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		reqID := middleware.RequestIDFrom(r.Context())
		model := chi.URLParam(r, "model")
		rate, ok := cache.Lookup(model)
		if !ok {
			writeError(w, mappedError{
				Status:  http.StatusNotFound,
				Type:    "invalid_request_error",
				Code:    "model_not_priced",
				Message: "No pricing entry for model " + model,
			}, reqID)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(dtoFromRate(rate))
	}
}

// pricingWriteRequest accepts either `*_per_1k` (float) or
// `*_per_1k_micro` (integer). Integer wins when both are supplied so
// the admin can avoid float drift if they care.
type pricingWriteRequest struct {
	Currency         string  `json:"currency"`
	InputPer1k       float64 `json:"input_per_1k"`
	OutputPer1k      float64 `json:"output_per_1k"`
	InputPer1kMicro  *int64  `json:"input_per_1k_micro"`
	OutputPer1kMicro *int64  `json:"output_per_1k_micro"`
}

func upsertPricing(cache *billing.PricingCache, recorder audit.Recorder, logger *zap.Logger) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		reqID := middleware.RequestIDFrom(r.Context())
		model := chi.URLParam(r, "model")
		if model == "" {
			writeError(w, mappedError{
				Status: http.StatusBadRequest, Type: "invalid_request_error",
				Code: "missing_model", Message: "Field 'model' is required in path",
			}, reqID)
			return
		}

		body, err := io.ReadAll(http.MaxBytesReader(nil, r.Body, 8<<10))
		if err != nil {
			writeError(w, mappedError{
				Status: http.StatusBadRequest, Type: "invalid_request_error",
				Message: "Failed to read request body",
			}, reqID)
			return
		}
		var req pricingWriteRequest
		if err := json.Unmarshal(body, &req); err != nil {
			writeError(w, mappedError{
				Status: http.StatusBadRequest, Type: "invalid_request_error",
				Message: "Malformed JSON: " + err.Error(),
			}, reqID)
			return
		}

		rate := billing.Rate{Model: model, Currency: req.Currency}
		if req.InputPer1kMicro != nil {
			rate.InputPer1kMicro = *req.InputPer1kMicro
		} else {
			rate.InputPer1kMicro = int64(req.InputPer1k * 1_000_000)
		}
		if req.OutputPer1kMicro != nil {
			rate.OutputPer1kMicro = *req.OutputPer1kMicro
		} else {
			rate.OutputPer1kMicro = int64(req.OutputPer1k * 1_000_000)
		}

		if err := cache.Set(r.Context(), rate); err != nil {
			logger.Warn("pricing upsert failed",
				zap.String("req_id", reqID),
				zap.String("model", model),
				zap.Error(err))
			writeError(w, mappedError{
				Status: http.StatusBadRequest, Type: "invalid_request_error",
				Message: err.Error(),
			}, reqID)
			return
		}

		// Re-fetch to pick up the canonical UpdatedAt the DB stamped.
		stored, _ := cache.Lookup(model)
		logger.Info("pricing upserted",
			zap.String("req_id", reqID),
			zap.String("model", model),
			zap.Int64("input_per_1k_micro", stored.InputPer1kMicro),
			zap.Int64("output_per_1k_micro", stored.OutputPer1kMicro))

		recordAudit(r.Context(), recorder, r, audit.ActionPricingUpsert, "pricing_rule", model,
			map[string]any{
				"currency":             stored.Currency,
				"input_per_1k_micro":   stored.InputPer1kMicro,
				"output_per_1k_micro":  stored.OutputPer1kMicro,
			}, logger)

		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(w).Encode(dtoFromRate(stored))
	}
}

func deletePricing(cache *billing.PricingCache, recorder audit.Recorder, logger *zap.Logger) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		reqID := middleware.RequestIDFrom(r.Context())
		model := chi.URLParam(r, "model")

		ok, err := cache.Delete(r.Context(), model)
		if err != nil {
			logger.Warn("pricing delete failed",
				zap.String("req_id", reqID),
				zap.String("model", model),
				zap.Error(err))
			writeError(w, mappedError{
				Status: http.StatusInternalServerError, Type: "internal_error",
				Message: "Failed to delete pricing",
			}, reqID)
			return
		}
		if !ok {
			writeError(w, mappedError{
				Status: http.StatusNotFound, Type: "invalid_request_error",
				Code: "model_not_priced", Message: "No pricing entry for model " + model,
			}, reqID)
			return
		}
		logger.Info("pricing deleted",
			zap.String("req_id", reqID),
			zap.String("model", model))
		recordAudit(r.Context(), recorder, r, audit.ActionPricingDelete, "pricing_rule", model, nil, logger)
		w.WriteHeader(http.StatusNoContent)
	}
}

// errPricingCacheMissing is unused by handlers but kept for the typed
// guard at server-mount time: callers wiring /admin without pricing
// configured should see a clear error instead of nil-deref panics.
var errPricingCacheMissing = errors.New("server: admin/pricing routes require a non-nil PricingCache")
