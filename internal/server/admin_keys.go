package server

import (
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"time"

	"github.com/go-chi/chi/v5"
	"go.uber.org/zap"

	"github.com/An-idd/x-beacon/internal/auth"
	"github.com/An-idd/x-beacon/internal/server/middleware"
)

// adminKeysHandlers builds the three /admin/keys routes. The Keystore
// is the single source of truth — both these endpoints and (in the
// future) xbctl share it, so the secret-generation policy and the
// cache-invalidation behavior live in one place.
//
// All routes are gated by RequireScope("admin","webui") at mount time,
// so handlers may assume an authenticated, authorized Principal on
// the request context.
func adminKeysHandlers(ks *auth.Keystore, logger *zap.Logger) chi.Router {
	r := chi.NewRouter()
	r.Get("/", listKeysHandler(ks, logger))
	r.Post("/", createKeyHandler(ks, logger))
	r.Post("/{id}/revoke", revokeKeyHandler(ks, logger))
	return r
}

// keyDTO is the on-the-wire projection. The plaintext secret is NEVER
// on this struct; it appears only in createKeyResponse and only on the
// initial POST response.
type keyDTO struct {
	ID           string              `json:"id"`
	IDPreview    string              `json:"id_preview"`
	Name         string              `json:"name"`
	HashHexShort string              `json:"hash_hex_short"`
	Scopes       map[string][]string `json:"scopes"`
	CreatedAt    time.Time           `json:"created_at"`
	LastUsedAt   *time.Time          `json:"last_used_at,omitempty"`
	RevokedAt    *time.Time          `json:"revoked_at,omitempty"`
}

func dtoFromRecord(r auth.KeyRecord) keyDTO {
	scopes := r.Scopes
	if scopes == nil {
		// Force a JSON object, not null, so the client side can do
		// `dto.scopes[cat]` without a presence check.
		scopes = map[string][]string{}
	}
	return keyDTO{
		ID:           r.ID,
		IDPreview:    r.IDPreview,
		Name:         r.Name,
		HashHexShort: r.HashHexShort,
		Scopes:       scopes,
		CreatedAt:    r.CreatedAt,
		LastUsedAt:   r.LastUsedAt,
		RevokedAt:    r.RevokedAt,
	}
}

func listKeysHandler(ks *auth.Keystore, logger *zap.Logger) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		reqID := middleware.RequestIDFrom(r.Context())
		q := r.URL.Query()

		opts := auth.ListOpts{
			NamePrefix:     q.Get("q"),
			IncludeRevoked: q.Get("include_revoked") == "true",
		}
		if v := q.Get("limit"); v != "" {
			n, err := strconv.Atoi(v)
			if err != nil || n < 1 {
				writeError(w, mappedError{
					Status: http.StatusBadRequest, Type: "invalid_request_error",
					Code: "invalid_limit", Message: "Field 'limit' must be a positive integer (max 200)",
				}, reqID)
				return
			}
			opts.Limit = n
		}
		if v := q.Get("offset"); v != "" {
			n, err := strconv.Atoi(v)
			if err != nil || n < 0 {
				writeError(w, mappedError{
					Status: http.StatusBadRequest, Type: "invalid_request_error",
					Code: "invalid_offset", Message: "Field 'offset' must be a non-negative integer",
				}, reqID)
				return
			}
			opts.Offset = n
		}

		records, total, err := ks.List(r.Context(), opts)
		if err != nil {
			logger.Error("admin keys list failed",
				zap.String("req_id", reqID), zap.Error(err))
			writeError(w, mappedError{
				Status: http.StatusInternalServerError, Type: "internal_error",
				Message: "Failed to list keys",
			}, reqID)
			return
		}

		items := make([]keyDTO, 0, len(records))
		for _, rec := range records {
			items = append(items, dtoFromRecord(rec))
		}

		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"object": "list",
			"items":  items,
			"total":  total,
		})
	}
}

// createKeyRequest is the inbound POST body. Scopes uses the JSONB
// shape on api_keys.scopes — `{"category": ["value", ...]}` — so we
// don't translate at the API boundary.
type createKeyRequest struct {
	Name   string              `json:"name"`
	Scopes map[string][]string `json:"scopes"`
}

// createKeyResponse is the one-shot return path for a freshly-created
// key. Secret appears here and nowhere else; document loudly that
// clients must persist it before disposing of the response.
type createKeyResponse struct {
	keyDTO
	Secret  string `json:"secret"`
	Warning string `json:"warning"`
}

func createKeyHandler(ks *auth.Keystore, logger *zap.Logger) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		reqID := middleware.RequestIDFrom(r.Context())

		var body createKeyRequest
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			writeError(w, mappedError{
				Status: http.StatusBadRequest, Type: "invalid_request_error",
				Message: "Malformed JSON: " + err.Error(),
			}, reqID)
			return
		}
		if body.Name == "" {
			writeError(w, mappedError{
				Status: http.StatusBadRequest, Type: "invalid_request_error",
				Code: "missing_name", Message: "Field 'name' is required",
			}, reqID)
			return
		}
		if len(body.Scopes) == 0 {
			writeError(w, mappedError{
				Status: http.StatusBadRequest, Type: "invalid_request_error",
				Code: "missing_scopes", Message: "Field 'scopes' must contain at least one entry",
			}, reqID)
			return
		}

		rec, secret, err := ks.Create(r.Context(), body.Name, body.Scopes)
		if err != nil {
			// Validation-shaped errors come back as plain `errors.New`;
			// surface them as 400 with the message verbatim so the
			// admin can correct without grepping logs. DB / encode
			// failures fall through to the 500 branch.
			//
			// We don't wrap with a sentinel because the source-of-truth
			// validation (scope format, name length) is in keystore.go
			// and changes there should propagate without a server.go
			// edit.
			if isValidationError(err) {
				writeError(w, mappedError{
					Status: http.StatusBadRequest, Type: "invalid_request_error",
					Code: "invalid_request", Message: err.Error(),
				}, reqID)
				return
			}
			logger.Error("admin keys create failed",
				zap.String("req_id", reqID), zap.Error(err))
			writeError(w, mappedError{
				Status: http.StatusInternalServerError, Type: "internal_error",
				Message: "Failed to create key",
			}, reqID)
			return
		}

		resp := createKeyResponse{
			keyDTO:  dtoFromRecord(rec),
			Secret:  secret,
			Warning: "Store this secret now. It is shown only once and cannot be recovered later.",
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(resp)
	}
}

func revokeKeyHandler(ks *auth.Keystore, logger *zap.Logger) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		reqID := middleware.RequestIDFrom(r.Context())
		id := chi.URLParam(r, "id")
		if id == "" {
			writeError(w, mappedError{
				Status: http.StatusBadRequest, Type: "invalid_request_error",
				Code: "missing_id", Message: "Path param 'id' is required",
			}, reqID)
			return
		}

		rec, err := ks.Revoke(r.Context(), id)
		if errors.Is(err, auth.ErrKeyNotFound) {
			writeError(w, mappedError{
				Status: http.StatusNotFound, Type: "invalid_request_error",
				Code: "key_not_found", Message: "No key with id " + id,
			}, reqID)
			return
		}
		if err != nil {
			logger.Error("admin keys revoke failed",
				zap.String("req_id", reqID), zap.String("id", id), zap.Error(err))
			writeError(w, mappedError{
				Status: http.StatusInternalServerError, Type: "internal_error",
				Message: "Failed to revoke key",
			}, reqID)
			return
		}

		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(dtoFromRecord(rec))
	}
}

// isValidationError flags errors whose message is safe to relay to
// admins (scope-format violations, name-length violations). These
// originate inside keystore.go and start with `auth: ` — anything
// without that prefix is treated as a 500.
//
// Avoiding a typed sentinel here is deliberate: every validation
// rule already has a unique message, and the messages are the
// useful debug signal for the admin. A typed sentinel would force
// us to either lose that signal or expose a sentinel per rule.
func isValidationError(err error) bool {
	if err == nil {
		return false
	}
	msg := err.Error()
	const prefix = "auth: "
	if len(msg) < len(prefix) || msg[:len(prefix)] != prefix {
		return false
	}
	// Heuristic: validation errors mention "must", "required", "invalid",
	// "has no values", or "<= 64". Internal/DB errors mention "query",
	// "scan", "insert", "encode" — words from package-internal layers.
	for _, kw := range []string{"must", "required", "invalid", "has no values"} {
		if contains(msg, kw) {
			return true
		}
	}
	return false
}

func contains(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}
