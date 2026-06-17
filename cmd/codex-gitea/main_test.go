package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"github.com/turning4th/codex-gitea/internal/config"
	"github.com/turning4th/codex-gitea/internal/model"
	"github.com/turning4th/codex-gitea/internal/store"
)

func newReadyzTestStore(t *testing.T) *store.Store {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	return st
}

func readyzTestConfig() *config.Config {
	return &config.Config{
		GiteaURL:      "https://git.example.com",
		GiteaToken:    "token",
		WebhookSecret: "secret",
		AdminPassword: "admin",
		CodexAuthMode: config.AuthModeAuthFile,
		Concurrency:   1,
		Timeout:       time.Minute,
		GiteaTimeout:  time.Minute,
	}
}

func TestReadyzOK(t *testing.T) {
	st := newReadyzTestStore(t)
	ctx := context.Background()
	job, _, err := st.EnqueueJob(ctx, &model.WebhookEvent{
		DeliveryID: "readyz-failed",
		Event:      model.EventPullRequest,
		Action:     "opened",
		PR:         model.PRRef{Owner: "acme", Repo: "widgets", Number: 12},
		Raw:        []byte(`{}`),
	})
	if err != nil {
		t.Fatalf("enqueue: %v", err)
	}
	if err := st.FinishJobDetailed(ctx, job.ID, model.JobFinish{
		Status: model.JobFailed, Error: "gitea timeout", ErrorType: model.ErrorTypeGitea, Retryable: true,
	}); err != nil {
		t.Fatalf("finish failed job: %v", err)
	}

	req := httptest.NewRequest(http.MethodGet, "/readyz", nil)
	rec := httptest.NewRecorder()
	readyzHandler(st, readyzTestConfig).ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
	}
	var resp struct {
		OK   bool `json:"ok"`
		DBOK bool `json:"db_ok"`
		Jobs struct {
			Total  int `json:"total"`
			Failed int `json:"failed"`
		} `json:"jobs"`
		LatestFailure *struct {
			ID        int64  `json:"id"`
			ErrorType string `json:"error_type"`
		} `json:"latest_failure"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode readyz: %v", err)
	}
	if !resp.OK || !resp.DBOK || resp.Jobs.Total != 1 || resp.Jobs.Failed != 1 {
		t.Fatalf("readyz response = %+v", resp)
	}
	if resp.LatestFailure == nil || resp.LatestFailure.ID != job.ID || resp.LatestFailure.ErrorType != string(model.ErrorTypeGitea) {
		t.Fatalf("latest failure = %+v, want failed job", resp.LatestFailure)
	}
}

func TestReadyzConfigWarnings(t *testing.T) {
	st := newReadyzTestStore(t)
	req := httptest.NewRequest(http.MethodGet, "/readyz", nil)
	rec := httptest.NewRecorder()
	readyzHandler(st, func() *config.Config {
		return &config.Config{CodexAuthMode: config.AuthModeAuthFile, Concurrency: 1, Timeout: time.Minute, GiteaTimeout: time.Minute}
	}).ServeHTTP(rec, req)
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
	}
	var resp struct {
		OK             bool     `json:"ok"`
		DBOK           bool     `json:"db_ok"`
		ConfigWarnings []string `json:"config_warnings"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode readyz: %v", err)
	}
	if resp.OK || !resp.DBOK || len(resp.ConfigWarnings) == 0 {
		t.Fatalf("readyz warnings response = %+v", resp)
	}
}
