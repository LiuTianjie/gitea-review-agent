package console

import (
	"context"
	"database/sql"
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

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

type StatusProvider func() map[string]StatusFunc

type ConfigProvider func() *config.Config

type SettingsApplyFunc func(context.Context, map[string]string) error

// Console serves the authenticated admin panel and its JSON API.
type Console struct {
	store           model.Store
	cfg             *config.Config
	configProvider  ConfigProvider
	codexHome       string
	statusFns       map[string]StatusFunc
	statusProvider  StatusProvider
	onSettingsApply SettingsApplyFunc
}

// New builds a Console. codexHome is the directory where auth.json uploads are written.
func New(store model.Store, cfg *config.Config, codexHome string, statusArgs ...any) *Console {
	c := &Console{store: store, cfg: cfg, codexHome: codexHome, statusFns: map[string]StatusFunc{}}
	for _, arg := range statusArgs {
		switch v := arg.(type) {
		case StatusFunc:
			c.statusFns["codex"] = v
		case map[string]StatusFunc:
			for name, fn := range v {
				c.statusFns[name] = fn
			}
		case StatusProvider:
			c.statusProvider = v
		case ConfigProvider:
			c.configProvider = v
		case SettingsApplyFunc:
			c.onSettingsApply = v
		}
	}
	return c
}

func (c *Console) currentConfig() *config.Config {
	if c.configProvider != nil {
		return c.configProvider()
	}
	if c.cfg != nil {
		return c.cfg.Clone()
	}
	return nil
}

func (c *Console) currentStatusFns() map[string]StatusFunc {
	if c.statusProvider != nil {
		return c.statusProvider()
	}
	out := make(map[string]StatusFunc, len(c.statusFns))
	for name, fn := range c.statusFns {
		out[name] = fn
	}
	return out
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
	mux.HandleFunc("GET /admin/api/effective-config", c.handleEffectiveConfig)
	mux.HandleFunc("GET /admin/api/jobs", c.handleJobs)
	mux.HandleFunc("GET /admin/api/jobs/stats", c.handleJobStats)
	mux.HandleFunc("GET /admin/api/jobs/{id}", c.handleJobDetail)
	mux.HandleFunc("POST /admin/api/analytics/reports", c.handleCreateAnalysisReport)
	mux.HandleFunc("GET /admin/api/analytics/reports/latest", c.handleLatestAnalysisReport)
	mux.HandleFunc("GET /admin/api/analytics/reports", c.handleListAnalysisReports)

	return consoleAuthMiddleware(func() string {
		if cfg := c.currentConfig(); cfg != nil {
			return cfg.AdminPassword
		}
		return ""
	}, mux)
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

	applied := make(map[string]string, len(updates))
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
		applied[k] = v
	}
	if c.onSettingsApply != nil && len(applied) > 0 {
		if err := c.onSettingsApply(r.Context(), applied); err != nil {
			writeError(w, http.StatusInternalServerError, "settings saved but reload failed: "+err.Error())
			return
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "updated": len(applied)})
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

	codexHome := c.codexHome
	if cfg := c.currentConfig(); cfg != nil && strings.TrimSpace(cfg.CodexHome) != "" {
		codexHome = cfg.CodexHome
	}
	if codexHome == "" {
		writeError(w, http.StatusInternalServerError, "codex home not configured")
		return
	}
	if err := os.MkdirAll(codexHome, 0o700); err != nil {
		writeError(w, http.StatusInternalServerError, "create codex home: "+err.Error())
		return
	}
	dest := filepath.Join(codexHome, "auth.json")
	if err := os.WriteFile(dest, raw, 0o600); err != nil {
		writeError(w, http.StatusInternalServerError, "write auth.json: "+err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "path": dest})
}

func (c *Console) handleStatus(w http.ResponseWriter, r *http.Request) {
	statusFns := c.currentStatusFns()
	if len(statusFns) == 0 {
		writeJSON(w, http.StatusOK, map[string]any{
			"ok":     false,
			"status": "status check not configured",
		})
		return
	}
	statuses := map[string]any{}
	allOK := true
	var lines []string
	for name, fn := range statusFns {
		status, err := fn(r.Context())
		ok := err == nil
		if !ok {
			allOK = false
			if strings.TrimSpace(status) == "" {
				status = err.Error()
			} else {
				status = strings.TrimSpace(status) + "\n\nerror: " + err.Error()
			}
		}
		statuses[name] = map[string]any{"ok": ok, "status": status}
		lines = append(lines, name+": "+status)
	}
	statusText := strings.Join(lines, "\n\n")
	if len(statusFns) == 1 {
		for _, v := range statuses {
			if item, ok := v.(map[string]any); ok {
				statusText, _ = item["status"].(string)
			}
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": allOK, "status": statusText, "reviewers": statuses})
}

func (c *Console) handleEffectiveConfig(w http.ResponseWriter, r *http.Request) {
	cfg := c.currentConfig()
	if cfg == nil {
		writeJSON(w, http.StatusOK, map[string]any{"ok": false, "error": "config not available"})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":                    true,
		"gitea_url":             cfg.GiteaURL,
		"gitea_token_set":       strings.TrimSpace(cfg.GiteaToken) != "",
		"webhook_secret_set":    strings.TrimSpace(cfg.WebhookSecret) != "",
		"model":                 cfg.Model,
		"codex_auth_mode":       cfg.CodexAuthMode,
		"codex_sandbox_mode":    cfg.CodexSandbox,
		"claude_enabled":        cfg.ClaudeEnabled,
		"claude_model":          cfg.ClaudeModel,
		"claude_api_key_set":    strings.TrimSpace(cfg.ClaudeAPIKey) != "",
		"claude_base_url":       cfg.ClaudeBaseURL,
		"claude_home":           cfg.ClaudeHome,
		"cc_switch_config_dir":  cfg.CCSwitchConfigDir,
		"cc_switch_provider_id": cfg.CCSwitchProvider,
		"claude_max_budget_usd": cfg.ClaudeMaxBudgetUSD,
		"concurrency":           cfg.Concurrency,
		"trigger_keywords":      strings.Join(cfg.TriggerKeywords, ","),
		"repo_allowlist":        strings.Join(cfg.RepoAllowlist, ","),
		"config_source":         os.Getenv("CONFIG_SOURCE"),
		"runtime_reload_note":   "保存设置会写入数据库；后续 review 会使用 reviewer 相关新值，监听地址和 worker 并发仍需重启服务。",
	})
}

// jobView is the JSON shape returned to the console, with PRRef flattened.
type jobView struct {
	ID         int64        `json:"id"`
	Owner      string       `json:"owner"`
	Repo       string       `json:"repo"`
	Number     int          `json:"number"`
	Event      string       `json:"event"`
	Action     string       `json:"action"`
	Status     string       `json:"status"`
	Attempts   int          `json:"attempts"`
	Error      string       `json:"error"`
	CreatedAt  string       `json:"created_at"`
	StartedAt  string       `json:"started_at"`
	FinishedAt string       `json:"finished_at"`
	SessionID  string       `json:"session_id"`
	LogCount   int          `json:"log_count"`
	Logs       []jobLogView `json:"logs"`
}

type jobLogView struct {
	Stage     string `json:"stage"`
	Message   string `json:"message"`
	CreatedAt string `json:"created_at"`
}

type jobsResponse struct {
	Jobs       []jobView `json:"jobs"`
	Page       int       `json:"page"`
	PageSize   int       `json:"page_size"`
	Total      int       `json:"total"`
	TotalPages int       `json:"total_pages"`
	HasMore    bool      `json:"has_more"`
}

type jobStatsView struct {
	TotalJobs    int     `json:"total_jobs"`
	ReviewJobs   int     `json:"review_jobs"`
	ReviewedJobs int     `json:"reviewed_jobs"`
	Done         int     `json:"done"`
	Failed       int     `json:"failed"`
	Running      int     `json:"running"`
	Pending      int     `json:"pending"`
	Superseded   int     `json:"superseded"`
	SuccessRate  float64 `json:"success_rate"`
}

func (c *Console) handleJobs(w http.ResponseWriter, r *http.Request) {
	page := parsePositiveInt(r.URL.Query().Get("page"), 1)
	pageSize := parsePositiveInt(r.URL.Query().Get("page_size"), 20)
	if pageSize > 100 {
		pageSize = 100
	}
	offset := (page - 1) * pageSize
	filter, err := parseJobFilter(r)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}

	jobs, err := c.store.ListJobsFiltered(r.Context(), filter, pageSize+1, offset)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	hasMore := len(jobs) > pageSize
	if hasMore {
		jobs = jobs[:pageSize]
	}
	out := make([]jobView, 0, len(jobs))
	for _, j := range jobs {
		out = append(out, toJobView(j))
	}
	writeJSON(w, http.StatusOK, jobsResponse{
		Jobs:     out,
		Page:     page,
		PageSize: pageSize,
		HasMore:  hasMore,
	})
}

func (c *Console) handleJobStats(w http.ResponseWriter, r *http.Request) {
	filter, err := parseJobFilter(r)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	stats, err := c.store.JobStatsFiltered(r.Context(), filter)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, toJobStatsView(stats))
}

func (c *Console) handleJobDetail(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil || id <= 0 {
		writeError(w, http.StatusBadRequest, "invalid job id")
		return
	}
	job, err := c.store.GetJobView(r.Context(), id)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			writeError(w, http.StatusNotFound, "job not found")
			return
		}
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, toJobView(*job))
}

func toJobView(j model.JobView) jobView {
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
		LogCount:  j.LogCount,
	}
	if !j.CreatedAt.IsZero() {
		jv.CreatedAt = j.CreatedAt.Format(timeLayout)
	}
	if j.StartedAt != nil && !j.StartedAt.IsZero() {
		jv.StartedAt = j.StartedAt.Format(timeLayout)
	}
	if j.FinishedAt != nil && !j.FinishedAt.IsZero() {
		jv.FinishedAt = j.FinishedAt.Format(timeLayout)
	}
	if len(j.Logs) > 0 {
		jv.Logs = make([]jobLogView, 0, len(j.Logs))
		for _, l := range j.Logs {
			lv := jobLogView{Stage: l.Stage, Message: l.Message}
			if !l.CreatedAt.IsZero() {
				lv.CreatedAt = l.CreatedAt.Format(timeLayout)
			}
			jv.Logs = append(jv.Logs, lv)
		}
		if jv.LogCount == 0 {
			jv.LogCount = len(j.Logs)
		}
	}
	return jv
}

func toJobStatsView(stats model.JobStats) jobStatsView {
	completed := stats.Done + stats.Failed
	successRate := 0.0
	if completed > 0 {
		successRate = float64(stats.Done) / float64(completed)
	}
	return jobStatsView{
		TotalJobs:    stats.TotalJobs,
		ReviewJobs:   stats.ReviewJobs,
		ReviewedJobs: stats.ReviewedJobs,
		Done:         stats.Done,
		Failed:       stats.Failed,
		Running:      stats.Running,
		Pending:      stats.Pending,
		Superseded:   stats.Superseded,
		SuccessRate:  successRate,
	}
}

type analysisReportView struct {
	ID        int64                 `json:"id"`
	CreatedAt string                `json:"created_at"`
	GiteaURL  string                `json:"gitea_url"`
	Summary   model.AnalysisSummary `json:"summary"`
}

func (c *Console) handleCreateAnalysisReport(w http.ResponseWriter, r *http.Request) {
	summary, err := c.store.BuildAnalysisSummary(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	report, err := c.store.CreateAnalysisReport(r.Context(), summary)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "report": c.toAnalysisReportView(*report)})
}

func (c *Console) handleLatestAnalysisReport(w http.ResponseWriter, r *http.Request) {
	report, err := c.store.LatestAnalysisReport(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if report == nil {
		writeJSON(w, http.StatusOK, map[string]any{"ok": true, "report": nil})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "report": c.toAnalysisReportView(*report)})
}

func (c *Console) handleListAnalysisReports(w http.ResponseWriter, r *http.Request) {
	limit := parsePositiveInt(r.URL.Query().Get("limit"), 20)
	reports, err := c.store.ListAnalysisReports(r.Context(), limit)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	out := make([]analysisReportView, 0, len(reports))
	for _, report := range reports {
		out = append(out, c.toAnalysisReportView(report))
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "reports": out})
}

func (c *Console) toAnalysisReportView(report model.AnalysisReport) analysisReportView {
	out := analysisReportView{ID: report.ID, Summary: report.Summary}
	if cfg := c.currentConfig(); cfg != nil {
		out.GiteaURL = strings.TrimRight(strings.TrimSpace(cfg.GiteaURL), "/")
	}
	if !report.CreatedAt.IsZero() {
		out.CreatedAt = report.CreatedAt.Format(timeLayout)
	}
	return out
}

func parsePositiveInt(raw string, fallback int) int {
	n, err := strconv.Atoi(strings.TrimSpace(raw))
	if err != nil || n <= 0 {
		return fallback
	}
	return n
}

func parseJobFilter(r *http.Request) (model.JobFilter, error) {
	var filter model.JobFilter
	from, err := parseOptionalTime(r.URL.Query().Get("created_from"), false)
	if err != nil {
		return filter, err
	}
	to, err := parseOptionalTime(r.URL.Query().Get("created_to"), true)
	if err != nil {
		return filter, err
	}
	if from != nil && to != nil && from.After(*to) {
		return filter, fmt.Errorf("created_from must be before created_to")
	}
	filter.CreatedFrom = from
	filter.CreatedTo = to
	return filter, nil
}

func parseOptionalTime(raw string, endOfDay bool) (*time.Time, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil, nil
	}
	if t, err := time.ParseInLocation("2006-01-02", raw, time.Local); err == nil {
		if endOfDay {
			t = t.Add(24*time.Hour - time.Nanosecond)
		}
		return &t, nil
	}
	layouts := []string{time.RFC3339Nano, time.RFC3339, "2006-01-02T15:04", "2006-01-02 15:04:05", "2006-01-02 15:04"}
	for _, layout := range layouts {
		if t, err := time.ParseInLocation(layout, raw, time.Local); err == nil {
			return &t, nil
		}
	}
	return nil, fmt.Errorf("invalid time %q", raw)
}
