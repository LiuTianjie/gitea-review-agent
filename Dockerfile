# syntax=docker/dockerfile:1

# ---------- build stage ----------
FROM golang:1.26 AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
# Static build so it runs on the slim runtime without libc surprises.
RUN CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags="-s -w" \
    -o /out/codex-gitea ./cmd/codex-gitea

# ---------- runtime stage ----------
FROM debian:bookworm-slim AS runtime

# git: gitcache mirrors/worktrees. node: runs the codex CLI. ca-certs+curl: TLS + healthcheck.
RUN apt-get update && apt-get install -y --no-install-recommends \
        git ca-certificates curl gnupg gosu \
    && curl -fsSL https://deb.nodesource.com/setup_22.x | bash - \
    && apt-get install -y --no-install-recommends nodejs \
    # Pin the codex CLI. NOTE: spiked behavior (thread.started event id,
    # generic `codex exec` honoring --output-schema, resume) verified on 0.133.0;
    # re-verify if you bump this.
    && npm install -g @openai/codex@0.135.0 \
    && apt-get purge -y gnupg && apt-get autoremove -y \
    && rm -rf /var/lib/apt/lists/*

# Non-root user; volumes are chowned to it in the entrypoint.
RUN useradd --create-home --uid 10001 app

COPY --from=build /out/codex-gitea /usr/local/bin/codex-gitea
COPY docker-entrypoint.sh /usr/local/bin/docker-entrypoint.sh
RUN chmod +x /usr/local/bin/docker-entrypoint.sh

ENV LISTEN_ADDR=:8080 \
    DB_PATH=/data/codex-gitea.db \
    CACHE_DIR=/cache \
    WORK_DIR=/work \
    CODEX_HOME=/codex-home \
    CODEX_AUTH_MODE=authfile

VOLUME ["/data", "/cache", "/work", "/codex-home"]
EXPOSE 8080

HEALTHCHECK --interval=30s --timeout=5s --start-period=10s --retries=3 \
    CMD curl -fsS http://localhost:8080/healthz || exit 1

ENTRYPOINT ["/usr/local/bin/docker-entrypoint.sh"]
CMD ["codex-gitea"]
