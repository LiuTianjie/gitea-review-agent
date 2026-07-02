#!/bin/sh
# Prepare writable volumes then drop to the unprivileged `app` user.
# CODEX_HOME and CC_SWITCH_CONFIG_DIR must stay writable for live provider state.
set -e

for d in /data /cache /work /codex-home /claude-home /cc-switch; do
	mkdir -p "$d"
	chown -R app:app "$d" 2>/dev/null || true
done

exec gosu app "$@"
