package console

import (
	"crypto/subtle"
	"net/http"
)

// basicAuthMiddleware guards next with HTTP Basic Auth. Any username is accepted;
// the password must match the configured admin password via a constant-time
// compare. When password is empty the console is disabled and every request is
// rejected with 503, so a misconfigured deployment never exposes an open panel.
func basicAuthMiddleware(password string, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if password == "" {
			http.Error(w, "console disabled, set ADMIN_PASSWORD", http.StatusServiceUnavailable)
			return
		}
		_, pass, ok := r.BasicAuth()
		// Compare regardless of ok so timing is independent of header presence.
		if !ok || subtle.ConstantTimeCompare([]byte(pass), []byte(password)) != 1 {
			w.Header().Set("WWW-Authenticate", `Basic realm="codex-gitea console", charset="UTF-8"`)
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		next.ServeHTTP(w, r)
	})
}
