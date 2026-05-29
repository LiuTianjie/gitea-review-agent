package console

import (
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"time"
)

// redacted is returned to the browser in place of a stored secret value, and is
// also the sentinel the browser sends back to mean "leave the secret unchanged".
const redacted = "***set***"

// timeLayout is the timestamp format used in JSON responses.
const timeLayout = time.RFC3339

// secretMarkers are substrings that classify a setting key as secret. Matching
// is case-insensitive and substring-based so keys like gitea_token,
// webhook_secret, codex_api_key, and admin_password are all covered.
var secretMarkers = []string{"token", "secret", "api_key", "password"}

// isSecretKey reports whether a settings key holds sensitive data that must be
// redacted in the UI and stored with the is_secret flag.
func isSecretKey(key string) bool {
	k := strings.ToLower(key)
	for _, m := range secretMarkers {
		if strings.Contains(k, m) {
			return true
		}
	}
	return false
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]any{"ok": false, "error": msg})
}

// readAll drains r fully, returning the bytes. It exists so handlers can read a
// request body once and both validate and persist the same bytes.
func readAll(r io.Reader) ([]byte, error) {
	return io.ReadAll(r)
}
