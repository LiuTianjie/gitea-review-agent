// Command codex-gitea runs the Gitea PR auto-review service.
package main

import (
	"context"
	"errors"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

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

	// DB settings override env defaults.
	if settings, err := st.AllSettings(context.Background()); err == nil {
		cfg.ApplyOverrides(settings)
	} else {
		logger.Printf("load settings: %v", err)
	}
	for _, w := range cfg.Warnings() {
		logger.Printf("config warning: %s", w)
	}
	if err := cfg.Validate(); err != nil {
		logger.Fatalf("invalid config: %v", err)
	}

	// Build modules.
	giteaClient := gitea.New(cfg.GiteaURL, cfg.GiteaToken, &http.Client{Timeout: 30 * time.Second})
	cache := gitcache.New(cfg.CacheDir, cfg.WorkDir, gitcache.WithToken(cfg.GiteaToken))

	codexOpts := codex.Options{
		CodexHome: cfg.CodexHome,
		Model:     cfg.Model,
		Timeout:   cfg.Timeout,
	}
	if cfg.CodexAuthMode == "apikey" {
		codexOpts.APIKey = cfg.CodexAPIKey
	}
	runner := codex.New(codexOpts)

	orch := &review.Orchestrator{
		Store:           st,
		Cache:           cache,
		Codex:           runner,
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

	cons := console.New(st, cfg, cfg.CodexHome, func(ctx context.Context) (string, error) {
		return runner.Status(ctx)
	})

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
