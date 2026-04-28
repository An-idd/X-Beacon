package middleware

import (
	"encoding/json"
	"net/http"

	"go.uber.org/zap"

	"github.com/An-idd/x-beacon/internal/auth"
)

// RequireScope returns middleware that gates downstream handlers on the
// authenticated principal carrying `value` under `category` in their
// scopes JSONB. Mount AFTER Auth so PrincipalFrom is populated.
//
// On a missing/insufficient scope: 403 forbidden + OpenAI-shaped error
// envelope. The handler never sees the request.
//
// Used by /admin/* routes (Week 7) so an ordinary chat-completion key
// can't write pricing.
func RequireScope(category, value string, logger *zap.Logger) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			p := auth.PrincipalFrom(r.Context())
			if !p.HasScope(category, value) {
				logger.Warn("scope denied",
					zap.String("req_id", RequestIDFrom(r.Context())),
					zap.String("path", r.URL.Path),
					zap.String("required_scope", category+":"+value),
					zap.String("principal_id", principalID(p)))
				writeScopeError(w, category, value)
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}

func principalID(p *auth.Principal) string {
	if p == nil {
		return ""
	}
	return p.ID
}

func writeScopeError(w http.ResponseWriter, category, value string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusForbidden)
	_ = json.NewEncoder(w).Encode(map[string]any{
		"error": map[string]any{
			"type":    "permission_error",
			"code":    "insufficient_scope",
			"message": "API key lacks required scope " + category + ":" + value,
		},
	})
}
