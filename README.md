# codex-gitea

Self-hosted service that auto-reviews Gitea pull requests with the Codex CLI
and, optionally, Claude Code or a MiniMax-compatible reviewer.
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
                              codex exec / claude --print (shared checkout)
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
- **Optional Claude reviewer** — set `CLAUDE_ENABLED=true` to post a second,
  independent Claude review. Codex and Claude share the same prepared worktree
  and diff for a job; one reviewer failing does not block the other.
- **Optional MiniMax reviewer** — set `MINIMAX_ENABLED=true` and either configure
  `MINIMAX_API_KEY` / `MINIMAX_BASE_URL` directly (for relay gateways) or provide
  a Claude app provider id in cc-switch. The service runs Claude Code through
  that MiniMax-compatible endpoint but stores/posts it as an independent
  `minimax` reviewer, so `/review minimax ...` targets its session.
- **React admin console** — `/admin` is a Vite/React app embedded into the Go
  binary. It covers task operations, runtime config, analytics, and project
  Skills; no standalone HTML file is required in production.
- **On-demand analytics** — the admin console can generate saved full-history
  analysis reports with ECharts trends for findings, success rate, severities,
  tags, reviewer/developer ownership, and multi-agent overlap.
- **Project Skills** — the console can distill recurring findings into a
  project-scoped Codex `SKILL.md`. Generation runs as a background task and the
  UI polls until it finishes. Generated Skills are publicly downloadable at
  `/skills/<owner>/<repo>/SKILL.md`, and the console provides a natural-language
  install instruction that can be copied directly.
- **Operational readiness** — `/healthz` stays a tiny liveness check, while
  `/readyz` returns DB/config/job counters and latest failure context for
  deployment diagnostics.

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
builds the admin console, runs Go tests, runs a Docker smoke test against the
new image, then builds a **multi-arch (amd64 + arm64)** image and pushes it to
`ghcr.io/<owner>/<repo>`. On Sealos, deploy that image and mount the six volumes
below. Make the GHCR package public, or add registry credentials in Sealos.

The smoke test starts the freshly built container and verifies:

- `/healthz` and `/readyz`
- login-protected `/admin` and embedded React assets
- task statistics, analytics trend, and Skill project APIs
- unauthenticated Skill download from `/skills/<owner>/<repo>/SKILL.md`

### Local (docker compose)

```bash
export ADMIN_PASSWORD=choose-a-strong-password
docker compose up -d
```

Volumes: `/data` (sqlite), `/cache` (mirrors), `/work` (worktrees),
`/codex-home` (codex auth + sessions, **writable**), `/claude-home`
(Claude Code state), and `/cc-switch` (provider/proxy config).

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
6. **Deployment check**: `/readyz` should return `{"ok":true,...}` after the
   first boot. If it returns 503, inspect `config_warnings` first.

## Usage

- Open a PR or push commits → automatic review.
- Comment `/review <question>` on a PR → answered with the prior review's context.
- Open `/admin` → inspect jobs, cancel pending jobs, rerun failed jobs, update
  runtime config, generate analytics, and evolve project Skills.
- Open `/skills/<owner>/<repo>/SKILL.md` → download a generated project Skill
  without console authentication.

## Admin console

### Tasks

The task tab shows recent jobs, status counters, retryable pending work, canceled
and superseded jobs. Pending jobs can be canceled from the detail panel; worker
finishes are conditional, so an old worker cannot overwrite a canceled or
superseded final state.

### Analytics

Analytics reports are stored snapshots. Reports include:

- finding and success-rate trends from existing review/finding data
- severity/status/reviewer/developer distributions
- high/critical recent issues with Gitea line links
- repeated titles and multi-agent overlap scoped to the same PR

### Skills

The Skill tab is project-scoped. It uses existing findings as evidence, includes
the previous generated Skill when present, and asks Codex to evolve the content
instead of starting from scratch.

Generation is asynchronous:

1. `POST /admin/api/skills/<owner>/<repo>/generate` returns a background `task_id`.
2. The UI polls `GET /admin/api/skills/<owner>/<repo>/generate/<task_id>`.
3. When the task finishes, the UI refreshes the version, content, download link,
   and copyable install instruction.

Install instruction format:

```text
请为 <owner>/<repo> 安装并使用这个项目缺陷预防 Skill：https://<host>/skills/<owner>/<repo>/SKILL.md
```

## GitHub Pages

`.github/workflows/pages.yml` publishes the static site in `docs/` to GitHub
Pages on every push to `main`. In the repository settings, set Pages source to
**GitHub Actions** if it is not already enabled.

Default Pages URL:

```text
https://liutianjie.github.io/gitea-review-agent/
```

## Configuration

Env vars (all optional except `ADMIN_PASSWORD`; the console can set the rest):

| Var | Default | Notes |
|-----|---------|-------|
| `ADMIN_PASSWORD` | — | required; protects `/admin` |
| `GITEA_URL` / `GITEA_TOKEN` | — | bot account |
| `GITEA_TIMEOUT` | `90s` | per Gitea API request; also configurable in console |
| `WEBHOOK_SECRET` | — | HMAC-SHA256 verification |
| `MODEL` | `gpt-5-codex` | codex model |
| `CODEX_AUTH_MODE` | `authfile` | or `apikey` |
| `CODEX_API_KEY` | — | apikey mode only (separately billed) |
| `CODEX_SANDBOX_MODE` | `read-only` | set `danger-full-access` only when the container blocks Codex's read-only sandbox |
| `CLAUDE_ENABLED` | `false` | enable the Claude reviewer |
| `CLAUDE_MODEL` | `sonnet` | Claude model/alias passed to Claude Code |
| `CLAUDE_API_KEY` | — | optional Anthropic or relay key; configurable in console |
| `CLAUDE_BASE_URL` | — | optional Anthropic-compatible relay URL; configurable in console |
| `CLAUDE_MAX_BUDGET_USD` | `0.3` | per Claude Code run budget cap; set `0` to disable |
| `CC_SWITCH_CONFIG_DIR` | `/cc-switch` | cc-switch provider/proxy config directory |
| `CC_SWITCH_PROVIDER_ID` | — | optional provider id to switch before Claude runs |
| `MINIMAX_ENABLED` | `false` | enable the MiniMax reviewer via Claude Code |
| `MINIMAX_PROVIDER_ID` | — | optional cc-switch Claude app provider id used before MiniMax review runs |
| `MINIMAX_API_KEY` | — | optional MiniMax/relay API key passed to Claude Code |
| `MINIMAX_BASE_URL` | — | optional MiniMax/relay Anthropic-compatible base URL |
| `MINIMAX_MODEL` | — | optional `claude --model` override; leave empty to use provider/relay defaults |
| `MINIMAX_MAX_BUDGET_USD` | `0.3` | per MiniMax/Claude Code run budget cap; set `0` to disable |
| `CONCURRENCY` | `5` | worker count |
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

The admin console is a Vite/React app embedded into the Go binary. CI and the
Dockerfile build it automatically before compiling the service, so deploying the
new image is enough. For local `go build` / `go test`, rebuild it after changing
console UI code:

```bash
cd internal/console/frontend
npm install
npm run build
```

Module layout: `internal/{webhook,queue,review,gitcache,codex,gitea,store,config,console}`,
wired in `cmd/codex-gitea/main.go`. Interfaces live in `internal/model/types.go`.
