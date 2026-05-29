package config

import (
	"testing"
	"time"
)

// setEnv sets env vars for the duration of a test and clears them after.
func setEnv(t *testing.T, kv map[string]string) {
	t.Helper()
	for k, v := range kv {
		t.Setenv(k, v)
	}
}

func TestLoadEnvDefaults(t *testing.T) {
	// t.Setenv on a key guarantees it is cleared afterward; explicitly clear the
	// ones we rely on being empty so a polluted environment can't break the test.
	for _, k := range []string{
		"LISTEN_ADDR", "DB_PATH", "CACHE_DIR", "WORK_DIR", "CODEX_HOME", "MODEL",
		"CODEX_AUTH_MODE", "CODEX_API_KEY", "GITEA_URL", "GITEA_TOKEN",
		"WEBHOOK_SECRET", "ADMIN_PASSWORD", "TRIGGER_KEYWORDS", "CONCURRENCY",
		"REPO_ALLOWLIST", "TIMEOUT", "SECRET_KEY",
	} {
		t.Setenv(k, "")
	}

	c := LoadEnv()

	if c.ListenAddr != DefaultListenAddr {
		t.Errorf("ListenAddr = %q, want %q", c.ListenAddr, DefaultListenAddr)
	}
	if c.DBPath != DefaultDBPath {
		t.Errorf("DBPath = %q, want %q", c.DBPath, DefaultDBPath)
	}
	if c.CacheDir != DefaultCacheDir {
		t.Errorf("CacheDir = %q, want %q", c.CacheDir, DefaultCacheDir)
	}
	if c.WorkDir != DefaultWorkDir {
		t.Errorf("WorkDir = %q, want %q", c.WorkDir, DefaultWorkDir)
	}
	if c.CodexHome != DefaultCodexHome {
		t.Errorf("CodexHome = %q, want %q", c.CodexHome, DefaultCodexHome)
	}
	if c.CodexAuthMode != AuthModeAuthFile {
		t.Errorf("CodexAuthMode = %q, want %q (default authfile)", c.CodexAuthMode, AuthModeAuthFile)
	}
	if c.Concurrency != DefaultConcurrency {
		t.Errorf("Concurrency = %d, want %d", c.Concurrency, DefaultConcurrency)
	}
	if c.Timeout != DefaultTimeout {
		t.Errorf("Timeout = %s, want %s", c.Timeout, DefaultTimeout)
	}
	if len(c.TriggerKeywords) != len(DefaultTriggerKeywords) {
		t.Fatalf("TriggerKeywords = %v, want %v", c.TriggerKeywords, DefaultTriggerKeywords)
	}
	for i, kw := range DefaultTriggerKeywords {
		if c.TriggerKeywords[i] != kw {
			t.Errorf("TriggerKeywords[%d] = %q, want %q", i, c.TriggerKeywords[i], kw)
		}
	}
	if len(c.RepoAllowlist) != 0 {
		t.Errorf("RepoAllowlist = %v, want empty", c.RepoAllowlist)
	}
}

func TestLoadEnvValues(t *testing.T) {
	setEnv(t, map[string]string{
		"LISTEN_ADDR":      ":9090",
		"DB_PATH":          "/tmp/db.sqlite",
		"CACHE_DIR":        "/tmp/cache",
		"WORK_DIR":         "/tmp/work",
		"CODEX_HOME":       "/tmp/codex",
		"GITEA_URL":        "https://git.example.com",
		"GITEA_TOKEN":      "tok-123",
		"WEBHOOK_SECRET":   "whsec",
		"MODEL":            "gpt-5",
		"CODEX_AUTH_MODE":  "apikey",
		"CODEX_API_KEY":    "sk-abc",
		"ADMIN_PASSWORD":   "hunter2",
		"TRIGGER_KEYWORDS": "/review, @bot , please-review",
		"CONCURRENCY":      "5",
		"REPO_ALLOWLIST":   "acme/widgets, acme/gadgets",
		"TIMEOUT":          "30s",
		"SECRET_KEY":       "key",
	})

	c := LoadEnv()

	if c.ListenAddr != ":9090" {
		t.Errorf("ListenAddr = %q", c.ListenAddr)
	}
	if c.GiteaURL != "https://git.example.com" {
		t.Errorf("GiteaURL = %q", c.GiteaURL)
	}
	if c.GiteaToken != "tok-123" {
		t.Errorf("GiteaToken = %q", c.GiteaToken)
	}
	if c.WebhookSecret != "whsec" {
		t.Errorf("WebhookSecret = %q", c.WebhookSecret)
	}
	if c.Model != "gpt-5" {
		t.Errorf("Model = %q", c.Model)
	}
	if c.CodexAuthMode != AuthModeAPIKey {
		t.Errorf("CodexAuthMode = %q, want apikey", c.CodexAuthMode)
	}
	if c.CodexAPIKey != "sk-abc" {
		t.Errorf("CodexAPIKey = %q", c.CodexAPIKey)
	}
	if c.AdminPassword != "hunter2" {
		t.Errorf("AdminPassword = %q", c.AdminPassword)
	}
	wantKW := []string{"/review", "@bot", "please-review"}
	if len(c.TriggerKeywords) != len(wantKW) {
		t.Fatalf("TriggerKeywords = %v, want %v", c.TriggerKeywords, wantKW)
	}
	for i, kw := range wantKW {
		if c.TriggerKeywords[i] != kw {
			t.Errorf("TriggerKeywords[%d] = %q, want %q", i, c.TriggerKeywords[i], kw)
		}
	}
	if c.Concurrency != 5 {
		t.Errorf("Concurrency = %d, want 5", c.Concurrency)
	}
	wantRepos := []string{"acme/widgets", "acme/gadgets"}
	if len(c.RepoAllowlist) != len(wantRepos) {
		t.Fatalf("RepoAllowlist = %v, want %v", c.RepoAllowlist, wantRepos)
	}
	for i, r := range wantRepos {
		if c.RepoAllowlist[i] != r {
			t.Errorf("RepoAllowlist[%d] = %q, want %q", i, c.RepoAllowlist[i], r)
		}
	}
	if c.Timeout != 30*time.Second {
		t.Errorf("Timeout = %s, want 30s", c.Timeout)
	}
	if c.SecretKey != "key" {
		t.Errorf("SecretKey = %q", c.SecretKey)
	}
}

func TestApplyOverrides(t *testing.T) {
	c := &Config{
		GiteaURL:        "https://env.example.com",
		GiteaToken:      "env-tok",
		Model:           "env-model",
		CodexAuthMode:   AuthModeAuthFile,
		Concurrency:     2,
		Timeout:         10 * time.Minute,
		TriggerKeywords: []string{"/review"},
	}

	c.ApplyOverrides(map[string]string{
		"gitea_url":        "https://db.example.com",
		"gitea_token":      "db-tok",
		"model":            "db-model",
		"codex_auth_mode":  "apikey",
		"codex_api_key":    "sk-db",
		"webhook_secret":   "db-secret",
		"trigger_keywords": "/lgtm,@review",
		"concurrency":      "8",
		"repo_allowlist":   "a/b",
		"timeout":          "5m",
	})

	if c.GiteaURL != "https://db.example.com" {
		t.Errorf("GiteaURL not overridden: %q", c.GiteaURL)
	}
	if c.GiteaToken != "db-tok" {
		t.Errorf("GiteaToken not overridden: %q", c.GiteaToken)
	}
	if c.Model != "db-model" {
		t.Errorf("Model not overridden: %q", c.Model)
	}
	if c.CodexAuthMode != AuthModeAPIKey {
		t.Errorf("CodexAuthMode not overridden: %q", c.CodexAuthMode)
	}
	if c.CodexAPIKey != "sk-db" {
		t.Errorf("CodexAPIKey not overridden: %q", c.CodexAPIKey)
	}
	if c.WebhookSecret != "db-secret" {
		t.Errorf("WebhookSecret not overridden: %q", c.WebhookSecret)
	}
	wantKW := []string{"/lgtm", "@review"}
	if len(c.TriggerKeywords) != len(wantKW) {
		t.Fatalf("TriggerKeywords = %v, want %v", c.TriggerKeywords, wantKW)
	}
	for i, kw := range wantKW {
		if c.TriggerKeywords[i] != kw {
			t.Errorf("TriggerKeywords[%d] = %q, want %q", i, c.TriggerKeywords[i], kw)
		}
	}
	if c.Concurrency != 8 {
		t.Errorf("Concurrency not overridden: %d", c.Concurrency)
	}
	if c.Timeout != 5*time.Minute {
		t.Errorf("Timeout not overridden: %s", c.Timeout)
	}
	if len(c.RepoAllowlist) != 1 || c.RepoAllowlist[0] != "a/b" {
		t.Errorf("RepoAllowlist not overridden: %v", c.RepoAllowlist)
	}
}

func TestApplyOverridesPartial(t *testing.T) {
	c := &Config{
		GiteaURL:   "https://env.example.com",
		GiteaToken: "env-tok",
		Model:      "env-model",
	}
	// Only override the URL; token and model must stay from env.
	c.ApplyOverrides(map[string]string{"gitea_url": "https://db.example.com"})

	if c.GiteaURL != "https://db.example.com" {
		t.Errorf("GiteaURL = %q, want db value", c.GiteaURL)
	}
	if c.GiteaToken != "env-tok" {
		t.Errorf("GiteaToken = %q, want env-tok (untouched)", c.GiteaToken)
	}
	if c.Model != "env-model" {
		t.Errorf("Model = %q, want env-model (untouched)", c.Model)
	}
}

func TestApplyOverridesNil(t *testing.T) {
	c := &Config{GiteaURL: "x"}
	c.ApplyOverrides(nil) // must not panic
	if c.GiteaURL != "x" {
		t.Errorf("GiteaURL changed by nil overrides: %q", c.GiteaURL)
	}
}

func TestValidateAuthFileMode(t *testing.T) {
	// authfile mode must pass even without an API key.
	c := &Config{
		CodexAuthMode: AuthModeAuthFile,
		Concurrency:   1,
		Timeout:       time.Minute,
	}
	if err := c.Validate(); err != nil {
		t.Errorf("Validate(authfile, no api key) = %v, want nil", err)
	}
}

func TestValidateAPIKeyMode(t *testing.T) {
	c := &Config{
		CodexAuthMode: AuthModeAPIKey,
		Concurrency:   1,
		Timeout:       time.Minute,
	}
	if err := c.Validate(); err == nil {
		t.Error("Validate(apikey, no key) = nil, want error")
	}

	c.CodexAPIKey = "sk-xyz"
	if err := c.Validate(); err != nil {
		t.Errorf("Validate(apikey, with key) = %v, want nil", err)
	}
}

func TestValidateBadMode(t *testing.T) {
	c := &Config{CodexAuthMode: "bogus", Concurrency: 1, Timeout: time.Minute}
	if err := c.Validate(); err == nil {
		t.Error("Validate(bogus mode) = nil, want error")
	}
}

func TestValidateBadConcurrencyAndTimeout(t *testing.T) {
	c := &Config{CodexAuthMode: AuthModeAuthFile, Concurrency: 0, Timeout: time.Minute}
	if err := c.Validate(); err == nil {
		t.Error("Validate(concurrency=0) = nil, want error")
	}
	c = &Config{CodexAuthMode: AuthModeAuthFile, Concurrency: 1, Timeout: 0}
	if err := c.Validate(); err == nil {
		t.Error("Validate(timeout=0) = nil, want error")
	}
}

func TestWarnings(t *testing.T) {
	c := &Config{CodexAuthMode: AuthModeAuthFile, Concurrency: 1, Timeout: time.Minute}
	w := c.Warnings()
	if len(w) == 0 {
		t.Error("Warnings() on empty config = none, want warnings about gitea/webhook/admin")
	}

	full := &Config{
		GiteaURL:      "https://x",
		GiteaToken:    "t",
		WebhookSecret: "s",
		AdminPassword: "p",
		CodexAuthMode: AuthModeAuthFile,
		Concurrency:   1,
		Timeout:       time.Minute,
	}
	if w := full.Warnings(); len(w) != 0 {
		t.Errorf("Warnings() on full config = %v, want none", w)
	}
}
