package server

import (
	"encoding/json"
	"net/http"

	"github.com/An-idd/x-beacon/internal/auth"
	"github.com/An-idd/x-beacon/internal/server/middleware"
)

// adminMeHandler returns the principal currently authenticated.
// Lightweight ID/label lookup so the WebUI header can display
// "logged in as <label>" instead of a key prefix.
//
// Response shape (note flat scopes mirroring /admin/keys):
//
//	{
//	  "id": "ak_01H...",
//	  "id_preview": "ak_01H2",
//	  "label": "team-frontend",
//	  "scopes": ["admin:webui", "admin:pricing"]
//	}
//
// No DB roundtrip — auth middleware already loaded the Principal
// onto the context. Cheap enough that the WebUI can re-fetch on
// every page navigation if it wants.
func adminMeHandler() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		reqID := middleware.RequestIDFrom(r.Context())
		p := auth.PrincipalFrom(r.Context())
		if p == nil {
			// Should be unreachable — Auth middleware would have
			// already returned 401 — but defend anyway so a wiring
			// regression doesn't surface as nil-pointer nonsense.
			writeError(w, mappedError{
				Status: http.StatusUnauthorized, Type: "invalid_request_error",
				Code: "no_principal", Message: "No authenticated principal on request",
			}, reqID)
			return
		}

		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"id":         p.ID,
			"id_preview": idPreviewMe(p.ID),
			"label":      p.Name,
			"scopes":     flattenScopes(p.Scopes),
		})
	}
}

// idPreviewMe is the same 8-char prefix convention used elsewhere
// (admin_keys.go has its own idPreview); declaring a local helper
// here avoids a cross-file export-just-for-this dance.
func idPreviewMe(id string) string {
	const w = 8
	if len(id) <= w {
		return id
	}
	return id[:w]
}
