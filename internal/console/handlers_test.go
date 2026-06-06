package console

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/turning4th/codex-gitea/internal/config"
	"github.com/turning4th/codex-gitea/internal/model"
	"github.com/turning4th/codex-gitea/internal/store"
)

const testPassword = "s3cret-pw"

// newTestConsole builds a Console backed by a real temp-db store. statusFn is
// optional. It returns the console and its codex home dir.
func newTestConsole(t *testing.T, statusFn StatusFunc) (*Console, string) {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "console.db"))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { st.Close() })

	codexHome := filepath.Join(t.TempDir(), "codex-home")
	cfg := &config.Config{AdminPassword: testPassword, CodexHome: codexHome}

	var c *Console
	if statusFn != nil {
		c = New(st, cfg, codexHome, statusFn)
	} else {
		c = New(st, cfg, codexHome)
	}
	return c, codexHome
}

// do issues a request against the console's full handler tree, optionally with
// a logged-in console session.
func do(t *testing.T, h http.Handler, method, path, body string, withAuth bool) *httptest.ResponseRecorder {
	t.Helper()
	var r *http.Request
	if body != "" {
		r = httptest.NewRequest(method, path, strings.NewReader(body))
	} else {
		r = httptest.NewRequest(method, path, nil)
	}
	if withAuth {
		c := loginCookie(t, h)
		r.AddCookie(c)
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w
}

func loginCookie(t *testing.T, h http.Handler) *http.Cookie {
	t.Helper()
	r := httptest.NewRequest("POST", "/admin/login", strings.NewReader("password="+testPassword))
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != http.StatusSeeOther {
		t.Fatalf("login: status=%d body=%s", w.Code, w.Body.String())
	}
	for _, c := range w.Result().Cookies() {
		if c.Name == consoleSessionCookie {
			return c
		}
	}
	t.Fatalf("login did not set %s cookie", consoleSessionCookie)
	return nil
}

func TestNoAuthRedirectsToLogin(t *testing.T) {
	c, _ := newTestConsole(t, nil)
	h := c.Routes()

	w := do(t, h, "GET", "/admin/", "", false)
	if w.Code != http.StatusSeeOther {
		t.Errorf("no auth page: status = %d, want 303", w.Code)
	}
	if loc := w.Header().Get("Location"); loc != "/admin/login" {
		t.Errorf("no auth redirect = %q, want /admin/login", loc)
	}
}

func TestNoAuthAPIRejectedWithoutBasicChallenge(t *testing.T) {
	c, _ := newTestConsole(t, nil)
	h := c.Routes()

	w := do(t, h, "GET", "/admin/api/settings", "", false)
	if w.Code != http.StatusUnauthorized {
		t.Errorf("no auth api: status = %d, want 401", w.Code)
	}
	if got := w.Header().Get("WWW-Authenticate"); got != "" {
		t.Errorf("unexpected Basic Auth challenge: %q", got)
	}
	if !strings.Contains(w.Body.String(), "unauthorized") {
		t.Errorf("api unauthorized body = %q", w.Body.String())
	}
}

func TestLoginPageServed(t *testing.T) {
	c, _ := newTestConsole(t, nil)
	h := c.Routes()

	w := do(t, h, "GET", "/admin/login", "", false)
	if w.Code != http.StatusOK {
		t.Fatalf("login page: status = %d, want 200", w.Code)
	}
	if !strings.Contains(w.Body.String(), "登录控制台") {
		t.Errorf("login page missing styled form")
	}
	if strings.Contains(w.Body.String(), "WWW-Authenticate") {
		t.Errorf("login page should not include Basic Auth marker")
	}
}

func TestWrongPasswordRejected(t *testing.T) {
	c, _ := newTestConsole(t, nil)
	h := c.Routes()

	r := httptest.NewRequest("POST", "/admin/login", strings.NewReader("password=wrong"))
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != http.StatusUnauthorized {
		t.Errorf("wrong password: status = %d, want 401", w.Code)
	}
	for _, c := range w.Result().Cookies() {
		if c.Name == consoleSessionCookie && c.Value != "" {
			t.Fatalf("wrong password set session cookie: %+v", c)
		}
	}
	if !strings.Contains(w.Body.String(), "密码不正确") {
		t.Errorf("wrong password body missing message")
	}
}

func TestLoginThenGetSettings(t *testing.T) {
	c, _ := newTestConsole(t, nil)
	h := c.Routes()

	w := do(t, h, "GET", "/admin/api/settings", "", true)
	if w.Code != http.StatusOK {
		t.Fatalf("authorized by login cookie: status = %d, want 200", w.Code)
	}
}

func TestConsoleDisabledWhenNoPassword(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "console.db"))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	cfg := &config.Config{AdminPassword: ""} // disabled
	c := New(st, cfg, t.TempDir())

	w := do(t, c.Routes(), "GET", "/admin/api/settings", "", false)
	if w.Code != http.StatusServiceUnavailable {
		t.Errorf("disabled console: status = %d, want 503", w.Code)
	}
	if !strings.Contains(w.Body.String(), "ADMIN_PASSWORD") {
		t.Errorf("disabled body = %q, want mention of ADMIN_PASSWORD", w.Body.String())
	}
}

func TestGetSettingsAuthorized(t *testing.T) {
	c, _ := newTestConsole(t, nil)
	h := c.Routes()

	w := do(t, h, "GET", "/admin/api/settings", "", true)
	if w.Code != http.StatusOK {
		t.Fatalf("authorized GET settings: status = %d, want 200", w.Code)
	}
	var m map[string]string
	if err := json.Unmarshal(w.Body.Bytes(), &m); err != nil {
		t.Fatalf("decode settings: %v (body=%s)", err, w.Body.String())
	}
}

func TestPostSettingsThenGetRedacts(t *testing.T) {
	c, _ := newTestConsole(t, nil)
	h := c.Routes()

	body := `{"settings":{"gitea_url":"https://git.example.com","gitea_token":"super-secret-token","model":"gpt-5-codex"}}`
	w := do(t, h, "POST", "/admin/api/settings", body, true)
	if w.Code != http.StatusOK {
		t.Fatalf("POST settings: status = %d, want 200 (body=%s)", w.Code, w.Body.String())
	}

	w = do(t, h, "GET", "/admin/api/settings", "", true)
	if w.Code != http.StatusOK {
		t.Fatalf("GET settings: status = %d", w.Code)
	}
	var m map[string]string
	if err := json.Unmarshal(w.Body.Bytes(), &m); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if m["gitea_url"] != "https://git.example.com" {
		t.Errorf("gitea_url = %q, want plain value", m["gitea_url"])
	}
	if m["model"] != "gpt-5-codex" {
		t.Errorf("model = %q", m["model"])
	}
	// Secret must be redacted, never echoed.
	if m["gitea_token"] != redacted {
		t.Errorf("gitea_token = %q, want %q (redacted)", m["gitea_token"], redacted)
	}
	if strings.Contains(w.Body.String(), "super-secret-token") {
		t.Errorf("response leaked secret token value: %s", w.Body.String())
	}
}

func TestPostSettingsRedactedSentinelKeepsSecret(t *testing.T) {
	c, _ := newTestConsole(t, nil)
	h := c.Routes()

	// Store a real secret first.
	do(t, h, "POST", "/admin/api/settings", `{"settings":{"gitea_token":"real-token"}}`, true)
	// Re-submit the redaction sentinel; the stored secret must be preserved.
	do(t, h, "POST", "/admin/api/settings", `{"settings":{"gitea_token":"`+redacted+`"}}`, true)

	val, ok, err := c.store.GetSetting(context.Background(), "gitea_token")
	if err != nil || !ok {
		t.Fatalf("GetSetting gitea_token: ok=%v err=%v", ok, err)
	}
	if val != "real-token" {
		t.Errorf("gitea_token overwritten by sentinel: %q, want real-token", val)
	}
}

func TestPostSettingsSingleKeyForm(t *testing.T) {
	c, _ := newTestConsole(t, nil)
	h := c.Routes()

	w := do(t, h, "POST", "/admin/api/settings", `{"key":"model","value":"gpt-5"}`, true)
	if w.Code != http.StatusOK {
		t.Fatalf("single-key POST: status = %d (body=%s)", w.Code, w.Body.String())
	}
	val, ok, _ := c.store.GetSetting(context.Background(), "model")
	if !ok || val != "gpt-5" {
		t.Errorf("model = %q ok=%v, want gpt-5", val, ok)
	}
}

func TestPostSettingsAppliesRuntimeHook(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "console.db"))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	cfg := &config.Config{AdminPassword: testPassword, CodexHome: filepath.Join(t.TempDir(), "codex-home")}
	var applied map[string]string
	c := New(st, cfg, cfg.CodexHome, SettingsApplyFunc(func(_ context.Context, updates map[string]string) error {
		applied = updates
		return nil
	}))
	h := c.Routes()

	w := do(t, h, "POST", "/admin/api/settings", `{"settings":{"claude_enabled":"true","claude_model":"sonnet"}}`, true)
	if w.Code != http.StatusOK {
		t.Fatalf("POST settings: status = %d (body=%s)", w.Code, w.Body.String())
	}
	if applied["claude_enabled"] != "true" || applied["claude_model"] != "sonnet" {
		t.Fatalf("runtime hook updates = %+v", applied)
	}
}

func TestPostSettingsEmptyRejected(t *testing.T) {
	c, _ := newTestConsole(t, nil)
	h := c.Routes()
	w := do(t, h, "POST", "/admin/api/settings", `{}`, true)
	if w.Code != http.StatusBadRequest {
		t.Errorf("empty settings POST: status = %d, want 400", w.Code)
	}
}

func TestAuthFileValidWritten0600(t *testing.T) {
	c, codexHome := newTestConsole(t, nil)
	h := c.Routes()

	authJSON := `{"tokens":{"refresh_token":"rt-abc","access_token":"at-xyz"},"OPENAI_API_KEY":null}`
	w := do(t, h, "POST", "/admin/api/authfile", authJSON, true)
	if w.Code != http.StatusOK {
		t.Fatalf("valid auth.json: status = %d, want 200 (body=%s)", w.Code, w.Body.String())
	}

	dest := filepath.Join(codexHome, "auth.json")
	info, err := os.Stat(dest)
	if err != nil {
		t.Fatalf("auth.json not written: %v", err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Errorf("auth.json perm = %o, want 600", perm)
	}
	got, err := os.ReadFile(dest)
	if err != nil {
		t.Fatalf("read auth.json: %v", err)
	}
	if !strings.Contains(string(got), "rt-abc") {
		t.Errorf("auth.json content missing refresh token: %s", got)
	}
}

func TestAuthFileMissingRefreshToken(t *testing.T) {
	c, codexHome := newTestConsole(t, nil)
	h := c.Routes()

	w := do(t, h, "POST", "/admin/api/authfile", `{"tokens":{"access_token":"only"}}`, true)
	if w.Code != http.StatusBadRequest {
		t.Errorf("missing refresh_token: status = %d, want 400 (body=%s)", w.Code, w.Body.String())
	}
	if _, err := os.Stat(filepath.Join(codexHome, "auth.json")); !os.IsNotExist(err) {
		t.Errorf("auth.json should not be written on validation failure")
	}
}

func TestAuthFileInvalidJSON(t *testing.T) {
	c, _ := newTestConsole(t, nil)
	h := c.Routes()
	w := do(t, h, "POST", "/admin/api/authfile", `not json at all`, true)
	if w.Code != http.StatusBadRequest {
		t.Errorf("invalid JSON: status = %d, want 400", w.Code)
	}
}

func TestStatusWithStub(t *testing.T) {
	c, _ := newTestConsole(t, func(ctx context.Context) (string, error) {
		return "Logged in using ChatGPT", nil
	})
	h := c.Routes()

	w := do(t, h, "GET", "/admin/api/status", "", true)
	if w.Code != http.StatusOK {
		t.Fatalf("status: code = %d", w.Code)
	}
	var resp struct {
		OK     bool   `json:"ok"`
		Status string `json:"status"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode status: %v", err)
	}
	if !resp.OK || resp.Status != "Logged in using ChatGPT" {
		t.Errorf("status resp = %+v, want ok+message", resp)
	}
}

func TestStatusNotConfigured(t *testing.T) {
	c, _ := newTestConsole(t, nil) // no status func
	h := c.Routes()

	w := do(t, h, "GET", "/admin/api/status", "", true)
	if w.Code != http.StatusOK {
		t.Fatalf("status: code = %d", w.Code)
	}
	var resp struct {
		OK     bool   `json:"ok"`
		Status string `json:"status"`
	}
	json.Unmarshal(w.Body.Bytes(), &resp)
	if resp.OK {
		t.Errorf("status ok = true, want false when unconfigured")
	}
}

func TestJobsEndpoint(t *testing.T) {
	c, _ := newTestConsole(t, nil)
	h := c.Routes()
	job, _, err := c.store.EnqueueJob(context.Background(), &model.WebhookEvent{
		DeliveryID: "console-job",
		Event:      model.EventPullRequest,
		Action:     "opened",
		PR:         model.PRRef{Owner: "acme", Repo: "widgets", Number: 17},
		Raw:        []byte(`{}`),
	})
	if err != nil {
		t.Fatalf("enqueue job: %v", err)
	}
	if err := c.store.AppendJobLog(context.Background(), job.ID, "codex", "review started"); err != nil {
		t.Fatalf("append job log: %v", err)
	}

	w := do(t, h, "GET", "/admin/api/jobs?page=1&page_size=20", "", true)
	if w.Code != http.StatusOK {
		t.Fatalf("jobs: code = %d", w.Code)
	}
	var resp struct {
		Jobs       []map[string]any `json:"jobs"`
		Page       int              `json:"page"`
		PageSize   int              `json:"page_size"`
		Total      int              `json:"total"`
		TotalPages int              `json:"total_pages"`
		HasMore    bool             `json:"has_more"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode jobs: %v (body=%s)", err, w.Body.String())
	}
	if resp.Page != 1 || resp.PageSize != 20 {
		t.Fatalf("jobs pagination = page %d size %d, want 1/20", resp.Page, resp.PageSize)
	}
	if resp.Jobs == nil {
		t.Fatalf("jobs response missing jobs array")
	}
	if len(resp.Jobs) != 1 || resp.Jobs[0]["log_count"].(float64) != 1 {
		t.Fatalf("jobs log summary = %+v, want one row with log_count=1", resp.Jobs)
	}
	if logs, ok := resp.Jobs[0]["logs"].([]any); ok && len(logs) != 0 {
		t.Fatalf("jobs list should not include full logs: %+v", logs)
	}

	w = do(t, h, "GET", "/admin/api/jobs/stats", "", true)
	if w.Code != http.StatusOK {
		t.Fatalf("job stats: code = %d body=%s", w.Code, w.Body.String())
	}
	var stats map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &stats); err != nil {
		t.Fatalf("decode job stats: %v", err)
	}
	if stats["total_jobs"].(float64) != 1 {
		t.Fatalf("job stats = %+v, want total_jobs=1", stats)
	}

	w = do(t, h, "GET", "/admin/api/jobs/"+strconv.FormatInt(job.ID, 10), "", true)
	if w.Code != http.StatusOK {
		t.Fatalf("job detail: code = %d body=%s", w.Code, w.Body.String())
	}
	var detail struct {
		LogCount int `json:"log_count"`
		Logs     []struct {
			Stage   string `json:"stage"`
			Message string `json:"message"`
		} `json:"logs"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &detail); err != nil {
		t.Fatalf("decode job detail: %v", err)
	}
	if detail.LogCount != 1 || len(detail.Logs) != 1 || detail.Logs[0].Stage != "codex" || detail.Logs[0].Message != "review started" {
		t.Fatalf("job detail logs = %+v, want codex/review started", detail)
	}
}

func TestAnalyticsReportEndpoints(t *testing.T) {
	c, _ := newTestConsole(t, nil)
	h := c.Routes()

	w := do(t, h, "POST", "/admin/api/analytics/reports", "", true)
	if w.Code != http.StatusOK {
		t.Fatalf("create report: code = %d body=%s", w.Code, w.Body.String())
	}
	var created struct {
		OK     bool           `json:"ok"`
		Report map[string]any `json:"report"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &created); err != nil {
		t.Fatalf("decode create report: %v", err)
	}
	if !created.OK || created.Report == nil {
		t.Fatalf("create report response mismatch: %+v", created)
	}

	w = do(t, h, "GET", "/admin/api/analytics/reports/latest", "", true)
	if w.Code != http.StatusOK {
		t.Fatalf("latest report: code = %d body=%s", w.Code, w.Body.String())
	}
	var latest struct {
		OK     bool           `json:"ok"`
		Report map[string]any `json:"report"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &latest); err != nil {
		t.Fatalf("decode latest report: %v", err)
	}
	if !latest.OK || latest.Report == nil {
		t.Fatalf("latest report response mismatch: %+v", latest)
	}
}

func TestIndexServed(t *testing.T) {
	c, _ := newTestConsole(t, nil)
	h := c.Routes()

	w := do(t, h, "GET", "/admin/", "", true)
	if w.Code != http.StatusOK {
		t.Fatalf("index: code = %d", w.Code)
	}
	if ct := w.Header().Get("Content-Type"); !strings.Contains(ct, "text/html") {
		t.Errorf("index content-type = %q, want text/html", ct)
	}
	if !strings.Contains(w.Body.String(), "gitea-review-agent 控制台") {
		t.Errorf("index body missing title")
	}
}

func TestIsSecretKey(t *testing.T) {
	secret := []string{"gitea_token", "webhook_secret", "codex_api_key", "admin_password"}
	for _, k := range secret {
		if !isSecretKey(k) {
			t.Errorf("isSecretKey(%q) = false, want true", k)
		}
	}
	plain := []string{"gitea_url", "model", "concurrency", "trigger_keywords", "timeout"}
	for _, k := range plain {
		if isSecretKey(k) {
			t.Errorf("isSecretKey(%q) = true, want false", k)
		}
	}
}

func TestParseOptionalTimeDateOnly(t *testing.T) {
	from, err := parseOptionalTime("2026-06-05", false)
	if err != nil {
		t.Fatalf("parse from date: %v", err)
	}
	to, err := parseOptionalTime("2026-06-05", true)
	if err != nil {
		t.Fatalf("parse to date: %v", err)
	}
	if from.Hour() != 0 || from.Minute() != 0 || from.Second() != 0 || from.Nanosecond() != 0 {
		t.Fatalf("from = %s, want start of day", from.Format(time.RFC3339Nano))
	}
	if to.Hour() != 23 || to.Minute() != 59 || to.Second() != 59 || to.Nanosecond() != int(time.Second-time.Nanosecond) {
		t.Fatalf("to = %s, want end of day", to.Format(time.RFC3339Nano))
	}
}
