#!/usr/bin/env bash
set -euo pipefail

image="${1:-codex-gitea:ci-smoke}"
name="codex-gitea-smoke-$$"
data_dir="$(mktemp -d)"
cookie_file="$(mktemp)"

cleanup() {
  docker rm -f "$name" >/dev/null 2>&1 || true
  rm -rf "$data_dir" "$cookie_file"
}
trap cleanup EXIT

docker build -t "$image" .

python3 - "$data_dir/codex-gitea.db" <<'PY'
import sqlite3
import sys

db = sqlite3.connect(sys.argv[1])
db.executescript(
    """
    CREATE TABLE IF NOT EXISTS project_skills(
      id INTEGER PRIMARY KEY,
      owner TEXT NOT NULL,
      repo TEXT NOT NULL,
      slug TEXT NOT NULL,
      title TEXT NOT NULL,
      content TEXT NOT NULL,
      version INTEGER NOT NULL DEFAULT 1,
      source_finding_count INTEGER NOT NULL DEFAULT 0,
      created_at TEXT,
      updated_at TEXT,
      UNIQUE(owner,repo)
    );
    INSERT INTO project_skills(owner,repo,slug,title,content,version,source_finding_count,created_at,updated_at)
    VALUES(
      'gaojiuaTech',
      'tifenxia-fe',
      'gaojiuaTech-tifenxia-fe-defect-skill',
      'tifenxia-fe smoke skill',
      '# tifenxia-fe smoke skill\n\nCodex docker smoke skill content.',
      1,
      1,
      datetime('now'),
      datetime('now')
    );
    """
)
db.commit()
db.close()
PY

docker run -d --name "$name" \
  -p 127.0.0.1::8080 \
  -v "$data_dir:/data" \
  -e ADMIN_PASSWORD=admin \
  -e LISTEN_ADDR=:8080 \
  -e GITEA_URL=https://gcode.gaojiua.com:3000 \
  -e GITEA_TOKEN=dummy-token \
  -e WEBHOOK_SECRET=dummy-secret \
  "$image" >/dev/null

host_port="$(docker port "$name" 8080/tcp | sed -E 's/.*:([0-9]+)$/\1/')"
base_url="http://127.0.0.1:${host_port}"

for _ in $(seq 1 30); do
  if curl -fsS "$base_url/healthz" >/dev/null 2>&1; then
    break
  fi
  sleep 1
done
curl -fsS "$base_url/healthz" | grep -qx "ok"

ready_json="$(curl -fsS "$base_url/readyz")"
python3 - "$ready_json" <<'PY'
import json
import sys

payload = json.loads(sys.argv[1])
if not payload.get("ok") or not payload.get("db_ok"):
    raise SystemExit(f"not ready: {payload}")
PY

curl -fsS -c "$cookie_file" \
  -H 'Content-Type: application/x-www-form-urlencoded' \
  -d 'password=admin' \
  "$base_url/admin/login" >/dev/null

index_html="$(curl -fsS -b "$cookie_file" "$base_url/admin/")"
asset_path="$(python3 - "$index_html" <<'PY'
import re
import sys

match = re.search(r'src="([^"]+/assets/[^"]+\.js)"', sys.argv[1])
if not match:
    raise SystemExit("admin bundle script not found")
print(match.group(1))
PY
)"
curl -fsSI -b "$cookie_file" "$base_url$asset_path" | grep -q "200 OK"

curl -fsS -b "$cookie_file" "$base_url/admin/api/jobs/stats" | grep -q '"retryable_pending"'
curl -fsS -b "$cookie_file" "$base_url/admin/api/analytics/trend?limit=12" | grep -q '"points"'
curl -fsS -b "$cookie_file" "$base_url/admin/api/skills/projects" | grep -q '"projects"'
curl -fsS "$base_url/skills/gaojiuaTech/tifenxia-fe/SKILL.md" | grep -q "Codex docker smoke skill content"

unauth_status="$(curl -sS -o /dev/null -w '%{http_code}' "$base_url/admin/api/jobs/stats")"
test "$unauth_status" = "401"

echo "docker smoke ok"
