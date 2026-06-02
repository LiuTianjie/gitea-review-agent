# codex-gitea

Self-hosted service that auto-reviews Gitea pull requests with the Codex CLI.
On each PR event it runs a **static** code review (read-only — never builds, runs,
or tests your code) and posts the findings back as inline review comments plus a
summary. Reviews are **stateful**: follow-up pushes and `/review` comments resume
the same Codex session, so it remembers what it already flagged.

## How it works

```
Gitea webhook ──▶ verify HMAC ──▶ enqueue (SQLite) ──▶ worker pool
                                                          │
                                   bare mirror + worktree (incremental fetch)
                                                          │
                              codex exec (read-only, --output-schema)
                                                          │
                              map findings to diff lines ──▶ Gitea review API
```

- **Stateful sessions** — first review creates a Codex thread; `synchronized`
  (new push) and `/review` comments `resume` it. Session id is parsed from the
  `thread.started` event in the `--json` stream.
- **Git cache** — one bare mirror per repo under `/cache`; events only fetch the
  delta, then check out a deterministic worktree. Big repos stay fast.
- **Read-only** — codex runs with `sandbox_mode=read-only` + `approval_policy=never`.
  It reviews the diff and never executes repo code.

## Auth: authfile (default) vs apikey

| Mode | Cost | Setup |
|------|------|-------|
| `authfile` (default) | reuses your ChatGPT subscription, **no extra API billing** | run `codex login` locally, upload the resulting `~/.codex/auth.json` via the `/admin` console |
| `apikey` | **separately billed** OpenAI Platform tokens | set `CODEX_API_KEY` + `CODEX_AUTH_MODE=apikey` |

In `authfile` mode the `/codex-home` volume **must be writable** so codex can
refresh its OAuth token in place between runs. Use the console's "check auth
status" button to catch a stale token early.

## Deploy

### Image (CI → GHCR → Sealos)

Pushing to `main` (or a `v*` tag) runs `.github/workflows/build-image.yml`, which
tests, then builds a **multi-arch (amd64 + arm64)** image and pushes it to
`ghcr.io/<owner>/codex-gitea`. On Sealos, deploy that image and mount the four
volumes below. Make the GHCR package public, or add registry credentials in Sealos.

### Local (docker compose)

```bash
export ADMIN_PASSWORD=choose-a-strong-password
docker compose up -d
```

Volumes: `/data` (sqlite), `/cache` (mirrors), `/work` (worktrees),
`/codex-home` (codex auth + sessions, **writable**).

## First-run checklist

1. **Codex creds**: `codex login` on your machine → upload `~/.codex/auth.json` at `/admin`.
2. **Gitea bot**: a Gitea user with a token scoped **repo read + PR write**; add it to private repos.
3. **Console password**: set `ADMIN_PASSWORD` (no password ⇒ `/admin` returns 503).
4. **Console config** (`/admin`): Gitea URL, bot token, webhook secret, model,
   trigger keywords, repo allowlist. DB settings override env and apply without a restart.
5. **Gitea webhook** (per repo or org-level):
   - URL `http://<host>:8080/webhook`, Content-Type `application/json`
   - Secret = the webhook secret you set in the console
   - Events: **Pull Request** + **Pull Request Sync** + **Issue Comment**

## Usage

- Open a PR or push commits → automatic review.
- Comment `/review <question>` on a PR → answered with the prior review's context.

## Configuration

Env vars (all optional except `ADMIN_PASSWORD`; the console can set the rest):

| Var | Default | Notes |
|-----|---------|-------|
| `ADMIN_PASSWORD` | — | required; protects `/admin` |
| `GITEA_URL` / `GITEA_TOKEN` | — | bot account |
| `WEBHOOK_SECRET` | — | HMAC-SHA256 verification |
| `MODEL` | `gpt-5-codex` | codex model |
| `CODEX_AUTH_MODE` | `authfile` | or `apikey` |
| `CODEX_API_KEY` | — | apikey mode only (separately billed) |
| `CODEX_SANDBOX_MODE` | `read-only` | set `danger-full-access` only when the container blocks Codex's read-only sandbox |
| `CONCURRENCY` | `2` | worker count |
| `TRIGGER_KEYWORDS` | `/review,@review` | comma-separated |
| `REPO_ALLOWLIST` | — | comma-separated `owner/repo`; empty = all |
| `TIMEOUT` | `30m` | per codex run |

## Security

- The `/admin` console can change tokens and upload credentials — **do not expose
  it publicly**. Keep it on a private network or behind a reverse-proxy auth layer.
- PR content is untrusted: codex runs read-only and its output is treated as data.
  Gitea/OpenAI tokens are never injected into the worktree environment.

## Development

```bash
go build ./...
go test ./...
```

Module layout: `internal/{webhook,queue,review,gitcache,codex,gitea,store,config,console}`,
wired in `cmd/codex-gitea/main.go`. Interfaces live in `internal/model/types.go`.
