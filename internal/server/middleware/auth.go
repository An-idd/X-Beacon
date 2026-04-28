package middleware

import (
	"encoding/json"
	"errors"
	"net/http"
	"strings"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.uber.org/zap"

	"github.com/An-idd/x-beacon/internal/auth"
	"github.com/An-idd/x-beacon/internal/observability"
)

// Auth is middleware that requires a valid bearer token on every request
// it protects. Mount it on the chi subrouter that hosts /v1/* — leave
// /healthz and /metrics outside.
//
// On success, the authenticated *auth.Principal is stored in request
// context and reachable via auth.PrincipalFrom.
//
// On failure: writes a 401 JSON error envelope (OpenAI-shaped) and logs
// at warn level with req_id. The body never echoes the supplied key.
func Auth(authn auth.Authenticator, logger *zap.Logger) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			key, ok := bearerToken(r.Header.Get("Authorization"))
			if !ok {
				writeAuthError(w, http.StatusUnauthorized, "missing_credentials",
					"Missing Authorization header (expected: Bearer <key>)")
				logger.Warn("auth rejected: missing credentials",
					zap.String("req_id", RequestIDFrom(r.Context())),
					zap.String("path", r.URL.Path))
				return
			}

			authCtx, span := observability.Tracer().Start(r.Context(), "auth.authenticate")
			principal, err := authn.Authenticate(authCtx, key)
			if err != nil {
				span.SetStatus(codes.Error, err.Error())
				span.End()
				switch {
				case errors.Is(err, auth.ErrMissingCredentials):
					writeAuthError(w, http.StatusUnauthorized, "missing_credentials",
						"Missing Authorization header (expected: Bearer <key>)")
				case errors.Is(err, auth.ErrInvalidCredentials):
					writeAuthError(w, http.StatusUnauthorized, "invalid_credentials",
						"Invalid API key")
				default:
					// Unexpected backend error (e.g. DB down in Week 4).
					writeAuthError(w, http.StatusInternalServerError, "internal_error",
						"Authentication backend error")
				}
				logger.Warn("auth rejected",
					zap.Error(err),
					zap.String("req_id", RequestIDFrom(r.Context())),
					zap.String("path", r.URL.Path))
				return
			}
			span.SetAttributes(attribute.String("principal.id", principal.ID))
			span.End()

			ctx := auth.WithPrincipal(r.Context(), principal)
			next.ServeHTTP(w, r.WithContext(ctx))
		})
	}
}

// bearerToken parses the Authorization header value and returns the bearer
// token. The match is case-insensitive on the scheme ("Bearer" / "bearer")
// and tolerates a single space; anything else returns ok=false.
func bearerToken(header string) (string, bool) {
	if header == "" {
		return "", false
	}
	const prefix = "bearer "
	if len(header) <= len(prefix) {
		return "", false
	}
	if !strings.EqualFold(header[:len(prefix)], prefix) {
		return "", false
	}
	token := strings.TrimSpace(header[len(prefix):])
	if token == "" {
		return "", false
	}
	return token, true
}

// writeAuthError serializes an OpenAI-compatible error envelope. Mirroring
// OpenAI's shape lets existing client SDKs surface the message naturally.
func writeAuthError(w http.ResponseWriter, status int, code, message string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]any{
		"error": map[string]any{
			"type":    "authentication_error",
			"code":    code,
			"message": message,
		},
	})
}
