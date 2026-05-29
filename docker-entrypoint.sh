#!/bin/sh
# Prepare writable volumes then drop to the unprivileged `app` user.
# CODEX_HOME must stay writable so codex can refresh its OAuth token in place.
set -e

for d in /data /cache /work /codex-home; do
	mkdir -p "$d"
	chown -R app:app "$d" 2>/dev/null || true
done

exec gosu app "$@"
