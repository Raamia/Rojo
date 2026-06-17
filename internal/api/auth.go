package api

import (
	"crypto/subtle"
	"net/http"
	"strings"
)

// healthPath is exempt from both auth and rate limiting so that orchestrator
// probes work without credentials and cannot be throttled. Declared once so the
// two bypasses cannot drift apart.
const healthPath = "/healthz"

func TokenAuth(expected string) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path == healthPath {
				next.ServeHTTP(w, r)
				return
			}
			header := r.Header.Get("Authorization")
			token := strings.TrimPrefix(header, "Bearer ")
			if token == header || subtle.ConstantTimeCompare([]byte(token), []byte(expected)) != 1 {
				WriteJSONError(w, http.StatusUnauthorized, "unauthorized")
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}
