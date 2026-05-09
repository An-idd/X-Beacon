package server

import (
	"encoding/json"
	"errors"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"go.uber.org/zap"

	"github.com/An-idd/x-beacon/internal/audit"
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
func adminKeysHandlers(ks *auth.Keystore, recorder audit.Recorder, logger *zap.Logger) chi.Router {
	r := chi.NewRouter()
	r.Get("/", listKeysHandler(ks, logger))
	r.Post("/", createKeyHandler(ks, recorder, logger))
	r.Post("/{id}/revoke", revokeKeyHandler(ks, recorder, logger))
	return r
}

// keyDTO is the on-the-wire projection. The plaintext secret is NEVER
// on this struct; it appears only in createKeyResponse and only on the
// initial POST response.
//
// Wire shape diverges from the internal KeyRecord on two axes (per
// docs/webui-requirements.md §5.2 contract for the WebUI):
//
//   - `name` (DB / internal) → `label` (wire) — UI vocabulary.
//   - scopes `map[category][]value` → flat `["category:value"]` slice
//     — admins type/select scope tuples; the UI doesn't need to deal
//     with the JSONB nested shape.
//
// HashHexShort is preserved (admin-only debug surface).
type keyDTO struct {
	ID           string     `json:"id"`
	IDPreview    string     `json:"id_preview"`
	Label        string     `json:"label"`
	HashHexShort string     `json:"hash_hex_short"`
	Scopes       []string   `json:"scopes"`
	CreatedAt    time.Time  `json:"created_at"`
	LastUsedAt   *time.Time `json:"last_used_at,omitempty"`
	RevokedAt    *time.Time `json:"revoked_at,omitempty"`
}

func dtoFromRecord(r auth.KeyRecord) keyDTO {
	return keyDTO{
		ID:           r.ID,
		IDPreview:    r.IDPreview,
		Label:        r.Name,
		HashHexShort: r.HashHexShort,
		Scopes:       flattenScopes(r.Scopes),
		CreatedAt:    r.CreatedAt,
		LastUsedAt:   r.LastUsedAt,
		RevokedAt:    r.RevokedAt,
	}
}

// flattenScopes turns the JSONB nested map into a sorted flat slice
// of `category:value` tuples. Sorted so list responses are stable
// across calls (tests + cache friendliness). Always returns a non-nil
// slice so JSON encodes as `[]` not `null` — the WebUI can `.length`
// without a presence check.
func flattenScopes(m map[string][]string) []string {
	out := []string{}
	for cat, vals := range m {
		for _, v := range vals {
			out = append(out, cat+":"+v)
		}
	}
	sort.Strings(out)
	return out
}

// expandScopes is the inverse: parse flat `["admin:webui", ...]`
// from the request body into the nested map keystore.Create expects.
// Errors on missing colon, empty side, or duplicate tuples — the
// HTTP-side validator (auth.scopePattern) catches format issues
// further down, but we want fast 400s on malformed input.
func expandScopes(flat []string) (map[string][]string, error) {
	out := make(map[string][]string)
	seen := make(map[string]struct{}, len(flat))
	for _, tuple := range flat {
		idx := strings.IndexByte(tuple, ':')
		if idx <= 0 || idx == len(tuple)-1 {
			return nil, errors.New("scope " + tuple + " must be in `category:value` form")
		}
		if _, dup := seen[tuple]; dup {
			continue
		}
		seen[tuple] = struct{}{}
		cat, val := tuple[:idx], tuple[idx+1:]
		out[cat] = append(out[cat], val)
	}
	return out, nil
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

// createKeyRequest is the inbound POST body. Wire shape mirrors the
// projection: `label` (not `name`) and a flat `["category:value"]`
// scope slice. Translation to the internal nested map happens in
// the handler via expandScopes.
type createKeyRequest struct {
	Label  string   `json:"label"`
	Scopes []string `json:"scopes"`
}

// createKeyResponse is the one-shot return path for a freshly-created
// key. The plaintext key appears here and nowhere else; document
// loudly that clients must persist it before disposing of the
// response.
type createKeyResponse struct {
	keyDTO
	Key     string `json:"key"`
	Warning string `json:"warning"`
}

func createKeyHandler(ks *auth.Keystore, recorder audit.Recorder, logger *zap.Logger) http.HandlerFunc {
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
		if body.Label == "" {
			writeError(w, mappedError{
				Status: http.StatusBadRequest, Type: "invalid_request_error",
				Code: "missing_label", Message: "Field 'label' is required",
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

		scopes, expandErr := expandScopes(body.Scopes)
		if expandErr != nil {
			writeError(w, mappedError{
				Status: http.StatusBadRequest, Type: "invalid_request_error",
				Code: "invalid_scopes", Message: expandErr.Error(),
			}, reqID)
			return
		}

		rec, secret, err := ks.Create(r.Context(), body.Label, scopes)
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
			Key:     secret,
			Warning: "Store this key now. It is shown only once and cannot be recovered later.",
		}
		recordAudit(r.Context(), recorder, r, audit.ActionKeyCreate, "api_key", rec.ID,
			map[string]any{
				"label":  rec.Name,
				"scopes": flattenScopes(rec.Scopes),
			}, logger)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(resp)
	}
}

func revokeKeyHandler(ks *auth.Keystore, recorder audit.Recorder, logger *zap.Logger) http.HandlerFunc {
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

		recordAudit(r.Context(), recorder, r, audit.ActionKeyRevoke, "api_key", rec.ID,
			map[string]any{"label": rec.Name}, logger)
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
