package httpserver

import (
	"crypto/subtle"
	"net/http"
	"strings"
)

// requireBearerToken wraps next so only requests carrying
// "Authorization: Bearer <token>" matching token are allowed through.
// Comparison uses subtle.ConstantTimeCompare to avoid timing attacks.
// An empty token always fails closed — without this guard, an unset token
// would accept any request with an empty Bearer value.
func requireBearerToken(token string, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if token == "" {
			http.Error(w, "server misconfigured: no auth token configured", http.StatusInternalServerError)
			return
		}

		const prefix = "Bearer "
		auth := r.Header.Get("Authorization")
		if !strings.HasPrefix(auth, prefix) {
			http.Error(w, "missing or malformed Authorization header", http.StatusUnauthorized)
			return
		}

		presented := strings.TrimPrefix(auth, prefix)
		if subtle.ConstantTimeCompare([]byte(presented), []byte(token)) != 1 {
			http.Error(w, "invalid token", http.StatusUnauthorized)
			return
		}

		next.ServeHTTP(w, r)
	})
}
