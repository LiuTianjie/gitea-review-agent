CREATE TABLE IF NOT EXISTS repos(
  id INTEGER PRIMARY KEY, owner TEXT, name TEXT, mirror_path TEXT,
  allowed INTEGER DEFAULT 1, UNIQUE(owner,name));
CREATE TABLE IF NOT EXISTS pulls(
  id INTEGER PRIMARY KEY, repo_id INTEGER REFERENCES repos(id),
  number INTEGER, author TEXT, session_id TEXT, head_sha TEXT, base_ref TEXT,
  last_review_id INTEGER, updated_at TEXT, UNIQUE(repo_id,number));
CREATE TABLE IF NOT EXISTS pull_reviewer_states(
  id INTEGER PRIMARY KEY, pull_id INTEGER REFERENCES pulls(id) ON DELETE CASCADE,
  agent TEXT, session_id TEXT, head_sha TEXT, base_ref TEXT,
  last_review_id INTEGER, updated_at TEXT,
  UNIQUE(pull_id,agent));
CREATE TABLE IF NOT EXISTS jobs(
  id INTEGER PRIMARY KEY, delivery_id TEXT UNIQUE, repo_id INTEGER,
  pr_number INTEGER, event TEXT, action TEXT, payload BLOB,
  status TEXT, attempts INTEGER DEFAULT 0, error TEXT,
  error_type TEXT, retryable INTEGER DEFAULT 0, next_attempt_at TEXT,
  created_at TEXT, started_at TEXT, finished_at TEXT);
CREATE INDEX IF NOT EXISTS jobs_claim ON jobs(status, repo_id);
CREATE INDEX IF NOT EXISTS jobs_status_id ON jobs(status, id);
CREATE INDEX IF NOT EXISTS jobs_status_repo_pr ON jobs(status, repo_id, pr_number);
CREATE INDEX IF NOT EXISTS jobs_created_desc ON jobs(created_at DESC, id DESC);
CREATE INDEX IF NOT EXISTS jobs_event_status ON jobs(event, status);
CREATE INDEX IF NOT EXISTS jobs_created_event_status ON jobs(created_at, event, status);
CREATE TABLE IF NOT EXISTS job_logs(
  id INTEGER PRIMARY KEY, job_id INTEGER REFERENCES jobs(id) ON DELETE CASCADE,
  stage TEXT, message TEXT, created_at TEXT);
CREATE INDEX IF NOT EXISTS job_logs_job ON job_logs(job_id, id);
CREATE TABLE IF NOT EXISTS review_runs(
  id INTEGER PRIMARY KEY, pull_id INTEGER REFERENCES pulls(id) ON DELETE CASCADE,
  job_id INTEGER REFERENCES jobs(id) ON DELETE SET NULL,
  agent TEXT, head_sha TEXT, status TEXT, error TEXT, error_type TEXT,
  finding_count INTEGER DEFAULT 0, started_at TEXT, finished_at TEXT);
CREATE INDEX IF NOT EXISTS review_runs_pull_agent ON review_runs(pull_id, agent, id);
CREATE TABLE IF NOT EXISTS findings(
  id INTEGER PRIMARY KEY, pull_id INTEGER REFERENCES pulls(id),
  review_run_id INTEGER REFERENCES review_runs(id) ON DELETE SET NULL,
  agent TEXT DEFAULT 'codex',
  fingerprint TEXT, path TEXT, line INTEGER, side TEXT, severity TEXT,
  title TEXT, body TEXT,
  gitea_comment_id INTEGER, review_id INTEGER,
  first_seen_sha TEXT, last_seen_sha TEXT, status TEXT,
  mapped_inline INTEGER DEFAULT 0, tags TEXT,
  UNIQUE(pull_id,fingerprint));
CREATE TABLE IF NOT EXISTS settings(
  key TEXT PRIMARY KEY, value TEXT, is_secret INTEGER DEFAULT 0, updated_at TEXT);
CREATE TABLE IF NOT EXISTS analysis_reports(
  id INTEGER PRIMARY KEY, summary_json TEXT NOT NULL, created_at TEXT);
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
  UNIQUE(owner,repo));
