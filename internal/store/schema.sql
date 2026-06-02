CREATE TABLE IF NOT EXISTS repos(
  id INTEGER PRIMARY KEY, owner TEXT, name TEXT, mirror_path TEXT,
  allowed INTEGER DEFAULT 1, UNIQUE(owner,name));
CREATE TABLE IF NOT EXISTS pulls(
  id INTEGER PRIMARY KEY, repo_id INTEGER REFERENCES repos(id),
  number INTEGER, session_id TEXT, head_sha TEXT, base_ref TEXT,
  last_review_id INTEGER, updated_at TEXT, UNIQUE(repo_id,number));
CREATE TABLE IF NOT EXISTS jobs(
  id INTEGER PRIMARY KEY, delivery_id TEXT UNIQUE, repo_id INTEGER,
  pr_number INTEGER, event TEXT, action TEXT, payload BLOB,
  status TEXT, attempts INTEGER DEFAULT 0, error TEXT,
  created_at TEXT, started_at TEXT, finished_at TEXT);
CREATE INDEX IF NOT EXISTS jobs_claim ON jobs(status, repo_id);
CREATE TABLE IF NOT EXISTS job_logs(
  id INTEGER PRIMARY KEY, job_id INTEGER REFERENCES jobs(id) ON DELETE CASCADE,
  stage TEXT, message TEXT, created_at TEXT);
CREATE INDEX IF NOT EXISTS job_logs_job ON job_logs(job_id, id);
CREATE TABLE IF NOT EXISTS findings(
  id INTEGER PRIMARY KEY, pull_id INTEGER REFERENCES pulls(id),
  fingerprint TEXT, path TEXT, line INTEGER, side TEXT, severity TEXT,
  title TEXT, body TEXT,
  gitea_comment_id INTEGER, review_id INTEGER,
  first_seen_sha TEXT, status TEXT, UNIQUE(pull_id,fingerprint));
CREATE TABLE IF NOT EXISTS settings(
  key TEXT PRIMARY KEY, value TEXT, is_secret INTEGER DEFAULT 0, updated_at TEXT);
