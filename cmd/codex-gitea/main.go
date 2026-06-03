// Command codex-gitea runs the Gitea PR auto-review service.
package main

import (
	"context"
	"errors"
	"log"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/turning4th/codex-gitea/internal/claude"
	"github.com/turning4th/codex-gitea/internal/codex"
	"github.com/turning4th/codex-gitea/internal/config"
	"github.com/turning4th/codex-gitea/internal/console"
	"github.com/turning4th/codex-gitea/internal/gitcache"
	"github.com/turning4th/codex-gitea/internal/gitea"
	"github.com/turning4th/codex-gitea/internal/model"
	"github.com/turning4th/codex-gitea/internal/queue"
	"github.com/turning4th/codex-gitea/internal/review"
	"github.com/turning4th/codex-gitea/internal/store"
	"github.com/turning4th/codex-gitea/internal/webhook"
)

func main() {
	logger := log.New(os.Stdout, "", log.LstdFlags|log.Lmsgprefix)

	cfg := config.LoadEnv()

	st, err := store.Open(cfg.DBPath)
	if err != nil {
		logger.Fatalf("open store: %v", err)
	}
	defer st.Close()

	// DB settings normally override env defaults. CONFIG_SOURCE=env makes
	// container env the source of truth for deployments that should not depend
	// on the mutable admin-console settings table.
	envConfigSource := strings.EqualFold(os.Getenv("CONFIG_SOURCE"), "env")
	if envConfigSource {
		logger.Printf("config source: env (database settings ignored)")
	} else {
		if settings, err := st.AllSettings(context.Background()); err == nil {
			cfg.ApplyOverrides(settings)
		} else {
			logger.Printf("load settings: %v", err)
		}
	}
	for _, w := range cfg.Warnings() {
		logger.Printf("config warning: %s", w)
	}
	if err := cfg.Validate(); err != nil {
		logger.Fatalf("invalid config: %v", err)
	}
	var cfgMu sync.RWMutex
	runtimeCfg := cfg.Clone()
	configSnapshot := func() *config.Config {
		cfgMu.RLock()
		defer cfgMu.RUnlock()
		return runtimeCfg.Clone()
	}
	applySettings := func(_ context.Context, updates map[string]string) error {
		if envConfigSource {
			return nil
		}
		cfgMu.Lock()
		defer cfgMu.Unlock()
		next := runtimeCfg.Clone()
		next.ApplyOverrides(updates)
		if err := next.Validate(); err != nil {
			return err
		}
		runtimeCfg = next
		return nil
	}

	// Build modules.
	giteaClient := gitea.New(cfg.GiteaURL, cfg.GiteaToken, &http.Client{Timeout: 30 * time.Second})
	cache := gitcache.New(cfg.CacheDir, cfg.WorkDir, gitcache.WithToken(cfg.GiteaToken))

	orch := &review.Orchestrator{
		Store:           st,
		Cache:           cache,
		ReviewersFunc:   func() []model.Reviewer { return buildReviewers(configSnapshot()) },
		Gitea:           giteaClient,
		TriggerKeywords: cfg.TriggerKeywords,
		RepoAllowlist:   cfg.RepoAllowlist,
		Logger:          logger,
	}

	q := queue.New(st, orch, cfg.Concurrency, logger)

	// Webhook -> enqueue (fast, then 200).
	onEvent := func(ctx context.Context, ev *model.WebhookEvent) error {
		if ev.Event == model.EventPullRequest && ev.Action == "synchronized" {
			if err := st.SupersedePending(ctx, ev.PR); err != nil {
				logger.Printf("supersede %s#%d: %v", ev.PR.Repo, ev.PR.Number, err)
			}
			q.CancelInFlight(ev.PR)
		}
		_, created, err := st.EnqueueJob(ctx, ev)
		if err != nil {
			return err
		}
		if created {
			q.Notify()
		}
		return nil
	}
	wh := webhook.NewHandler(cfg.WebhookSecret, onEvent)

	statusProvider := func() map[string]console.StatusFunc {
		return buildStatusFns(configSnapshot())
	}
	cons := console.New(st, cfg, cfg.CodexHome,
		console.ConfigProvider(configSnapshot),
		console.StatusProvider(statusProvider),
		console.SettingsApplyFunc(applySettings),
	)

	root := http.NewServeMux()
	root.Handle("/webhook", wh)
	root.Handle("/healthz", wh)
	root.Handle("/admin/", cons.Routes())

	srv := &http.Server{Addr: cfg.ListenAddr, Handler: root}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	go func() {
		logger.Printf("worker pool starting (concurrency=%d)", cfg.Concurrency)
		if err := q.Run(ctx); err != nil && !errors.Is(err, context.Canceled) {
			logger.Printf("queue stopped: %v", err)
		}
	}()

	go func() {
		logger.Printf("listening on %s (webhook=/webhook, console=/admin/)", cfg.ListenAddr)
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			logger.Fatalf("http server: %v", err)
		}
	}()

	<-ctx.Done()
	logger.Printf("shutting down...")
	shutCtx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	_ = srv.Shutdown(shutCtx)
}

func buildReviewers(cfg *config.Config) []model.Reviewer {
	if cfg == nil {
		return nil
	}
	codexOpts := codex.Options{
		CodexHome:   cfg.CodexHome,
		Model:       cfg.Model,
		SandboxMode: cfg.CodexSandbox,
		Timeout:     cfg.Timeout,
	}
	if cfg.CodexAuthMode == config.AuthModeAPIKey {
		codexOpts.APIKey = cfg.CodexAPIKey
	}
	reviewers := []model.Reviewer{codex.New(codexOpts)}
	if cfg.ClaudeEnabled {
		reviewers = append(reviewers, claude.New(claude.Options{
			Model:             cfg.ClaudeModel,
			APIKey:            cfg.ClaudeAPIKey,
			BaseURL:           cfg.ClaudeBaseURL,
			ClaudeHome:        cfg.ClaudeHome,
			CCSwitchConfigDir: cfg.CCSwitchConfigDir,
			CCSwitchProvider:  cfg.CCSwitchProvider,
			Timeout:           cfg.Timeout,
		}))
	}
	return reviewers
}

func buildStatusFns(cfg *config.Config) map[string]console.StatusFunc {
	statusFns := map[string]console.StatusFunc{}
	for _, reviewer := range buildReviewers(cfg) {
		r := reviewer
		statusFns[r.Name()] = func(ctx context.Context) (string, error) { return r.Status(ctx) }
	}
	return statusFns
}
