package console

import (
	"context"
	_ "embed"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strings"

	"github.com/turning4th/codex-gitea/internal/config"
	"github.com/turning4th/codex-gitea/internal/model"
)

// indexHTML is the single-page console UI, embedded so the binary is self
// contained and works offline (no CDN dependencies).
//
//go:embed static/index.html
var indexHTML []byte

// StatusFunc reports codex auth status. The orchestrator injects
// codex.Runner.Status; tests inject a stub. A nil StatusFunc means "not wired".
type StatusFunc func(context.Context) (string, error)

// Console serves the authenticated admin panel and its JSON API.
type Console struct {
	store     model.Store
	cfg       *config.Config
	codexHome string
	statusFn  StatusFunc
}

// New builds a Console. statusFn is optional; pass zero or one function. Only the
// first is used. codexHome is the directory where auth.json uploads are written.
func New(store model.Store, cfg *config.Config, codexHome string, statusFn ...StatusFunc) *Console {
	c := &Console{store: store, cfg: cfg, codexHome: codexHome}
	if len(statusFn) > 0 {
		c.statusFn = statusFn[0]
	}
	return c
}

// Routes returns the console handler tree wrapped in Basic Auth. Mount it under
// whatever prefix the caller chooses; the internal mux uses absolute /admin/...
// paths so the page's fetch() calls resolve correctly.
func (c *Console) Routes() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /admin/", c.handleIndex)
	mux.HandleFunc("GET /admin/api/settings", c.handleGetSettings)
	mux.HandleFunc("POST /admin/api/settings", c.handlePostSettings)
	mux.HandleFunc("POST /admin/api/authfile", c.handlePostAuthFile)
	mux.HandleFunc("GET /admin/api/status", c.handleStatus)
	mux.HandleFunc("GET /admin/api/jobs", c.handleJobs)

	password := ""
	if c.cfg != nil {
		password = c.cfg.AdminPassword
	}
	return basicAuthMiddleware(password, mux)
}

// ---------- handlers ----------

func (c *Console) handleIndex(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Write(indexHTML)
}

// handleGetSettings returns all settings with secret values redacted. Secret
// keys never leak their value to the browser; the UI only learns whether a key
// is set.
func (c *Console) handleGetSettings(w http.ResponseWriter, r *http.Request) {
	all, err := c.store.AllSettings(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	out := make(map[string]string, len(all))
	for k, v := range all {
		if isSecretKey(k) {
			if strings.TrimSpace(v) != "" {
				out[k] = redacted
			} else {
				out[k] = ""
			}
			continue
		}
		out[k] = v
	}
	writeJSON(w, http.StatusOK, out)
}

// settingsPayload accepts either a single {key,value} pair or a {settings:{...}}
// batch. Both forms may be present; both are applied.
type settingsPayload struct {
	Key      string            `json:"key"`
	Value    string            `json:"value"`
	Settings map[string]string `json:"settings"`
}

func (c *Console) handlePostSettings(w http.ResponseWriter, r *http.Request) {
	var p settingsPayload
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&p); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON body: "+err.Error())
		return
	}

	updates := map[string]string{}
	for k, v := range p.Settings {
		updates[k] = v
	}
	if p.Key != "" {
		updates[p.Key] = p.Value
	}
	if len(updates) == 0 {
		writeError(w, http.StatusBadRequest, "no settings provided")
		return
	}

	for k, v := range updates {
		// Never store the redaction placeholder: it means "leave the existing
		// secret unchanged", so the UI can re-submit a form without wiping secrets.
		if isSecretKey(k) && v == redacted {
			continue
		}
		if err := c.store.SetSetting(r.Context(), k, v, isSecretKey(k)); err != nil {
			writeError(w, http.StatusInternalServerError, err.Error())
			return
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "updated": len(updates)})
}

// authFile is the minimal shape we validate before persisting auth.json. The
// presence of tokens.refresh_token is what authfile-mode codex login requires.
type authFile struct {
	Tokens struct {
		RefreshToken string `json:"refresh_token"`
	} `json:"tokens"`
}

func (c *Console) handlePostAuthFile(w http.ResponseWriter, r *http.Request) {
	raw, err := readAll(http.MaxBytesReader(w, r.Body, 1<<20))
	if err != nil {
		writeError(w, http.StatusBadRequest, "read body: "+err.Error())
		return
	}
	// Must be valid JSON.
	var af authFile
	if err := json.Unmarshal(raw, &af); err != nil {
		writeError(w, http.StatusBadRequest, "auth.json is not valid JSON: "+err.Error())
		return
	}
	if strings.TrimSpace(af.Tokens.RefreshToken) == "" {
		writeError(w, http.StatusBadRequest, "auth.json missing tokens.refresh_token")
		return
	}

	if c.codexHome == "" {
		writeError(w, http.StatusInternalServerError, "codex home not configured")
		return
	}
	if err := os.MkdirAll(c.codexHome, 0o700); err != nil {
		writeError(w, http.StatusInternalServerError, "create codex home: "+err.Error())
		return
	}
	dest := filepath.Join(c.codexHome, "auth.json")
	if err := os.WriteFile(dest, raw, 0o600); err != nil {
		writeError(w, http.StatusInternalServerError, "write auth.json: "+err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "path": dest})
}

func (c *Console) handleStatus(w http.ResponseWriter, r *http.Request) {
	if c.statusFn == nil {
		writeJSON(w, http.StatusOK, map[string]any{
			"ok":     false,
			"status": "status check not configured",
		})
		return
	}
	status, err := c.statusFn(r.Context())
	if err != nil {
		writeJSON(w, http.StatusOK, map[string]any{
			"ok":     false,
			"status": err.Error(),
		})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "status": status})
}

// jobView is the JSON shape returned to the console, with PRRef flattened.
type jobView struct {
	ID         int64  `json:"id"`
	Owner      string `json:"owner"`
	Repo       string `json:"repo"`
	Number     int    `json:"number"`
	Event      string `json:"event"`
	Action     string `json:"action"`
	Status     string `json:"status"`
	Attempts   int    `json:"attempts"`
	Error      string `json:"error"`
	CreatedAt  string `json:"created_at"`
	FinishedAt string `json:"finished_at"`
	SessionID  string `json:"session_id"`
}

func (c *Console) handleJobs(w http.ResponseWriter, r *http.Request) {
	jobs, err := c.store.ListJobs(r.Context(), 50)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	out := make([]jobView, 0, len(jobs))
	for _, j := range jobs {
		jv := jobView{
			ID:        j.ID,
			Owner:     j.PR.Owner,
			Repo:      j.PR.Repo,
			Number:    j.PR.Number,
			Event:     string(j.Event),
			Action:    j.Action,
			Status:    string(j.Status),
			Attempts:  j.Attempts,
			Error:     j.Error,
			SessionID: j.SessionID,
		}
		if !j.CreatedAt.IsZero() {
			jv.CreatedAt = j.CreatedAt.Format(timeLayout)
		}
		if j.FinishedAt != nil && !j.FinishedAt.IsZero() {
			jv.FinishedAt = j.FinishedAt.Format(timeLayout)
		}
		out = append(out, jv)
	}
	writeJSON(w, http.StatusOK, out)
}
