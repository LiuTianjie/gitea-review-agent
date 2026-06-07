package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/turning4th/codex-gitea/internal/model"
)

// EnqueueJob serializes ev into a pending job. The delivery_id UNIQUE constraint
// deduplicates redeliveries: if a row with the same delivery_id already exists,
// it is returned with created=false and no error.
func (s *Store) EnqueueJob(ctx context.Context, ev *model.WebhookEvent) (*model.Job, bool, error) {
	payload, err := json.Marshal(ev)
	if err != nil {
		return nil, false, fmt.Errorf("marshal webhook event: %w", err)
	}

	repoID, err := ensureRepo(ctx, s.db, ev.PR.Owner, ev.PR.Repo)
	if err != nil {
		return nil, false, err
	}

	now := nowRFC3339()
	res, err := s.db.ExecContext(ctx,
		`INSERT INTO jobs(delivery_id,repo_id,pr_number,event,action,payload,status,attempts,created_at)
		 VALUES(?,?,?,?,?,?,?,0,?)
		 ON CONFLICT(delivery_id) DO NOTHING`,
		ev.DeliveryID, repoID, ev.PR.Number, string(ev.Event), ev.Action,
		payload, string(model.JobPending), now)
	if err != nil {
		return nil, false, fmt.Errorf("insert job: %w", err)
	}

	affected, err := res.RowsAffected()
	if err != nil {
		return nil, false, fmt.Errorf("rows affected: %w", err)
	}
	if affected == 0 {
		// Duplicate delivery: fetch and return the existing job, created=false.
		job, err := s.getJobByDeliveryID(ctx, ev.DeliveryID)
		if err != nil {
			return nil, false, err
		}
		return job, false, nil
	}

	id, err := res.LastInsertId()
	if err != nil {
		return nil, false, fmt.Errorf("last insert id: %w", err)
	}
	job, err := s.GetJob(ctx, id)
	if err != nil {
		return nil, false, err
	}
	return job, true, nil
}

// SupersedePending marks all pending (and running) jobs for pr as superseded,
// so a newer event for the same PR takes priority.
func (s *Store) SupersedePending(ctx context.Context, pr model.PRRef) error {
	repoID, ok, err := lookupRepoID(ctx, s.db, pr.Owner, pr.Repo)
	if err != nil {
		return fmt.Errorf("supersede lookup repo: %w", err)
	}
	if !ok {
		return nil
	}
	_, err = s.db.ExecContext(ctx,
		`UPDATE jobs SET status=? WHERE repo_id=? AND pr_number=? AND status IN(?,?)`,
		string(model.JobSuperseded), repoID, pr.Number,
		string(model.JobPending), string(model.JobRunning))
	if err != nil {
		return fmt.Errorf("supersede jobs: %w", err)
	}
	return nil
}

// ClaimJob atomically transitions the oldest pending job to running and returns
// it. Returns (nil, nil) when no pending job exists. The UPDATE ... RETURNING
// statement keeps concurrent workers from claiming the same row.
func (s *Store) ClaimJob(ctx context.Context) (*model.Job, error) {
	var id int64
	err := s.db.QueryRowContext(ctx,
		`UPDATE jobs
		 SET status=?, attempts=attempts+1, started_at=?, finished_at=NULL,
		     error='', error_type='', retryable=0, next_attempt_at=NULL
		 WHERE id = (
		   SELECT j.id
		   FROM jobs j
		   WHERE j.status=?
		     AND (j.next_attempt_at IS NULL OR j.next_attempt_at='' OR j.next_attempt_at<=?)
		     AND NOT EXISTS (
		       SELECT 1 FROM jobs r
		       WHERE r.status=?
		         AND r.repo_id=j.repo_id
		         AND r.pr_number=j.pr_number
		     )
		   ORDER BY j.id LIMIT 1
		 )
		 RETURNING id`,
		string(model.JobRunning), nowRFC3339(),
		string(model.JobPending), nowRFC3339(), string(model.JobRunning)).Scan(&id)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("claim job: %w", err)
	}

	job, err := scanJob(ctx, s.db, id)
	if err != nil {
		return nil, err
	}
	return job, nil
}

// FinishJob sets a terminal status (done/failed/superseded) and the error msg.
func (s *Store) FinishJob(ctx context.Context, id int64, status model.JobStatus, errMsg string) error {
	return s.FinishJobDetailed(ctx, id, model.JobFinish{Status: status, Error: errMsg})
}

// FinishJobDetailed updates a job with error classification and optional retry
// scheduling. A pending finish represents a delayed retry rather than a
// terminal result.
func (s *Store) FinishJobDetailed(ctx context.Context, id int64, finish model.JobFinish) error {
	status := finish.Status
	if status == "" {
		status = model.JobFailed
	}
	errorType := finish.ErrorType
	if errorType == "" && strings.TrimSpace(finish.Error) != "" {
		errorType = model.ErrorTypeUnknown
	}
	retryable := 0
	if finish.Retryable {
		retryable = 1
	}
	nextAttempt := ""
	if finish.NextAttemptAt != nil && !finish.NextAttemptAt.IsZero() {
		nextAttempt = finish.NextAttemptAt.UTC().Format(time.RFC3339)
	}

	finishedAt := nowRFC3339()
	startedAtSQL := "started_at"
	if status == model.JobPending {
		finishedAt = ""
		startedAtSQL = "NULL"
	}
	_, err := s.db.ExecContext(ctx,
		`UPDATE jobs SET status=?, error=?, error_type=?, retryable=?, next_attempt_at=?,
		   started_at=`+startedAtSQL+`, finished_at=? WHERE id=?`,
		string(status), finish.Error, string(errorType), retryable, nextAttempt, finishedAt, id)
	if err != nil {
		return fmt.Errorf("finish job: %w", err)
	}
	return nil
}

// RerunJob creates a fresh pending job from an existing job payload. The new
// delivery id is synthetic so webhook delivery deduplication remains intact.
func (s *Store) RerunJob(ctx context.Context, id int64) (*model.Job, error) {
	original, err := scanJob(ctx, s.db, id)
	if err != nil {
		return nil, err
	}
	var repoID int64
	if err := s.db.QueryRowContext(ctx, `SELECT repo_id FROM jobs WHERE id=?`, id).Scan(&repoID); err != nil {
		return nil, fmt.Errorf("lookup rerun repo: %w", err)
	}
	now := nowRFC3339()
	deliveryID := fmt.Sprintf("%s:rerun:%d", original.DeliveryID, time.Now().UnixNano())
	res, err := s.db.ExecContext(ctx,
		`INSERT INTO jobs(delivery_id,repo_id,pr_number,event,action,payload,status,attempts,error,error_type,retryable,next_attempt_at,created_at)
		 VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		deliveryID, repoID, original.PR.Number, string(original.Event), original.Action, original.Payload,
		string(model.JobPending), 0, "", "", 0, "", now)
	if err != nil {
		return nil, fmt.Errorf("insert rerun job: %w", err)
	}
	newID, err := res.LastInsertId()
	if err != nil {
		return nil, fmt.Errorf("rerun job id: %w", err)
	}
	return scanJob(ctx, s.db, newID)
}

// AppendJobLog adds a progress entry visible in the admin console.
func (s *Store) AppendJobLog(ctx context.Context, jobID int64, stage, message string) error {
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO job_logs(job_id,stage,message,created_at) VALUES(?,?,?,?)`,
		jobID, stage, message, nowRFC3339())
	if err != nil {
		return fmt.Errorf("append job log: %w", err)
	}
	return nil
}

// RecoverRunning resets all running jobs back to pending. Called on boot to
// requeue work that was in flight when the process died.
func (s *Store) RecoverRunning(ctx context.Context) error {
	_, err := s.db.ExecContext(ctx,
		`UPDATE jobs SET status=?, started_at=NULL WHERE status=?`,
		string(model.JobPending), string(model.JobRunning))
	if err != nil {
		return fmt.Errorf("recover running: %w", err)
	}
	return nil
}

// ListJobs returns a page of jobs (newest first) as console read models, joined
// to repo owner/name and the PR's session id.
func (s *Store) ListJobs(ctx context.Context, limit, offset int) ([]model.JobView, error) {
	return s.ListJobsFiltered(ctx, model.JobFilter{}, limit, offset)
}

// ListJobsFiltered returns a page of jobs matching filter (newest first), joined
// to repo owner/name and the PR's session id.
func (s *Store) ListJobsFiltered(ctx context.Context, filter model.JobFilter, limit, offset int) ([]model.JobView, error) {
	if limit <= 0 {
		limit = 20
	}
	if limit > 101 {
		limit = 101
	}
	if offset < 0 {
		offset = 0
	}
	where, args := jobFilterWhere(filter)
	args = append(args, limit, offset)
	rows, err := s.db.QueryContext(ctx,
		jobViewSelect(where)+`
		 ORDER BY page.created_at DESC, page.id DESC`, args...)
	if err != nil {
		return nil, fmt.Errorf("list jobs: %w", err)
	}
	out, err := scanJobViewRows(rows)
	if err != nil {
		return nil, err
	}
	return out, nil
}

func jobViewSelect(where string) string {
	return `WITH page AS (
  SELECT j.id, j.delivery_id, j.repo_id, j.pr_number, j.event, j.action, j.payload,
         j.status, j.attempts, j.error, j.error_type, j.retryable,
         j.created_at, j.started_at, j.finished_at, j.next_attempt_at
  FROM jobs j ` + where + `
  ORDER BY j.created_at DESC, j.id DESC LIMIT ? OFFSET ?
)
SELECT page.id, r.owner, r.name, page.pr_number, page.event, page.action,
       page.status, page.attempts,
       CASE
         WHEN length(COALESCE(page.error,'')) > 300 THEN substr(page.error, 1, 300) || '\n...(truncated; open task detail for full error)'
         ELSE page.error
       END,
       COALESCE(page.error_type,''), COALESCE(page.retryable,0),
       page.created_at, page.started_at, page.finished_at, page.next_attempt_at,
       COALESCE(p.session_id,''),
       (SELECT COUNT(*) FROM job_logs l WHERE l.job_id=page.id)
FROM page
LEFT JOIN repos r ON r.id = page.repo_id
LEFT JOIN pulls p ON p.repo_id = page.repo_id AND p.number = page.pr_number`
}

func scanJobViewRows(rows *sql.Rows) ([]model.JobView, error) {
	defer rows.Close()

	var out []model.JobView
	for rows.Next() {
		var (
			v           model.JobView
			owner       sql.NullString
			name        sql.NullString
			event       string
			status      string
			errMsg      sql.NullString
			errorType   sql.NullString
			retryable   int
			createdAt   string
			startedAt   sql.NullString
			finishedAt  sql.NullString
			nextAttempt sql.NullString
			sessionID   string
		)
		if err := rows.Scan(&v.ID, &owner, &name, &v.PR.Number, &event, &v.Action,
			&status, &v.Attempts, &errMsg, &errorType, &retryable, &createdAt, &startedAt, &finishedAt,
			&nextAttempt, &sessionID, &v.LogCount); err != nil {
			return nil, fmt.Errorf("scan job view: %w", err)
		}
		v.PR.Owner = owner.String
		v.PR.Repo = name.String
		v.Event = model.EventType(event)
		v.Status = model.JobStatus(status)
		v.Error = errMsg.String
		v.ErrorType = model.ErrorType(errorType.String)
		v.Retryable = retryable != 0
		v.CreatedAt = parseTime(createdAt)
		v.StartedAt = nullableTime(startedAt)
		v.FinishedAt = nullableTime(finishedAt)
		v.NextAttemptAt = nullableTime(nextAttempt)
		v.SessionID = sessionID
		out = append(out, v)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate job views: %w", err)
	}
	return out, nil
}

// CountJobs returns the total number of queued jobs.
func (s *Store) CountJobs(ctx context.Context) (int, error) {
	return s.CountJobsFiltered(ctx, model.JobFilter{})
}

// CountJobsFiltered returns the total number of queued jobs matching filter.
func (s *Store) CountJobsFiltered(ctx context.Context, filter model.JobFilter) (int, error) {
	var n int
	where, args := jobFilterWhere(filter)
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM jobs j `+where, args...).Scan(&n); err != nil {
		return 0, fmt.Errorf("count jobs: %w", err)
	}
	return n, nil
}

// JobStats returns aggregate job counts for the console dashboard.
func (s *Store) JobStats(ctx context.Context) (model.JobStats, error) {
	return s.JobStatsFiltered(ctx, model.JobFilter{})
}

// JobStatsFiltered returns aggregate job counts matching filter.
func (s *Store) JobStatsFiltered(ctx context.Context, filter model.JobFilter) (model.JobStats, error) {
	var stats model.JobStats
	where, args := jobFilterWhere(filter)
	rows, err := s.db.QueryContext(ctx,
		`SELECT j.event, j.status, COUNT(*)
		 FROM jobs j `+where+`
		 GROUP BY j.event, j.status`, args...)
	if err != nil {
		return model.JobStats{}, fmt.Errorf("job stats: %w", err)
	}
	defer rows.Close()

	for rows.Next() {
		var event, status string
		var n int
		if err := rows.Scan(&event, &status, &n); err != nil {
			return model.JobStats{}, fmt.Errorf("scan job stats: %w", err)
		}
		stats.TotalJobs += n
		if event == string(model.EventPullRequest) {
			stats.ReviewJobs += n
			if status == string(model.JobDone) {
				stats.ReviewedJobs += n
			}
		}
		switch status {
		case string(model.JobDone):
			stats.Done += n
		case string(model.JobFailed):
			stats.Failed += n
		case string(model.JobRunning):
			stats.Running += n
		case string(model.JobPending):
			stats.Pending += n
		case string(model.JobSuperseded):
			stats.Superseded += n
		}
	}
	if err := rows.Err(); err != nil {
		return model.JobStats{}, fmt.Errorf("iterate job stats: %w", err)
	}
	return stats, nil
}

func jobFilterWhere(filter model.JobFilter) (string, []any) {
	var clauses []string
	var args []any
	if filter.CreatedFrom != nil && !filter.CreatedFrom.IsZero() {
		clauses = append(clauses, "j.created_at >= ?")
		args = append(args, filter.CreatedFrom.UTC().Format(time.RFC3339))
	}
	if filter.CreatedTo != nil && !filter.CreatedTo.IsZero() {
		clauses = append(clauses, "j.created_at <= ?")
		args = append(args, filter.CreatedTo.UTC().Format(time.RFC3339))
	}
	if len(clauses) == 0 {
		return "", args
	}
	return "WHERE " + strings.Join(clauses, " AND "), args
}

func (s *Store) listJobLogs(ctx context.Context, jobID int64) ([]model.JobLog, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT id, stage, message, created_at FROM job_logs WHERE job_id=? ORDER BY id`,
		jobID)
	if err != nil {
		return nil, fmt.Errorf("list job logs: %w", err)
	}
	defer rows.Close()

	var out []model.JobLog
	for rows.Next() {
		var (
			l         model.JobLog
			createdAt string
		)
		if err := rows.Scan(&l.ID, &l.Stage, &l.Message, &createdAt); err != nil {
			return nil, fmt.Errorf("scan job log: %w", err)
		}
		l.CreatedAt = parseTime(createdAt)
		out = append(out, l)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate job logs: %w", err)
	}
	return out, nil
}

// GetJobView returns one console job view with its full log timeline.
func (s *Store) GetJobView(ctx context.Context, id int64) (*model.JobView, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT j.id, r.owner, r.name, j.pr_number, j.event, j.action,
       j.status, j.attempts, j.error, COALESCE(j.error_type,''), COALESCE(j.retryable,0),
       j.created_at, j.started_at, j.finished_at, j.next_attempt_at,
       COALESCE(p.session_id,''),
       (SELECT COUNT(*) FROM job_logs l WHERE l.job_id=j.id)
FROM jobs j
LEFT JOIN repos r ON r.id = j.repo_id
LEFT JOIN pulls p ON p.repo_id = j.repo_id AND p.number = j.pr_number
WHERE j.id=?`, id)
	if err != nil {
		return nil, fmt.Errorf("get job view: %w", err)
	}
	out, err := scanJobViewRows(rows)
	if err != nil {
		return nil, err
	}
	if len(out) == 0 {
		return nil, sql.ErrNoRows
	}
	logs, err := s.listJobLogs(ctx, id)
	if err != nil {
		return nil, err
	}
	out[0].Logs = logs
	out[0].LogCount = len(logs)
	return &out[0], nil
}

// GetJob returns the full job by id.
func (s *Store) GetJob(ctx context.Context, id int64) (*model.Job, error) {
	return scanJob(ctx, s.db, id)
}

// getJobByDeliveryID returns the job with the given delivery_id.
func (s *Store) getJobByDeliveryID(ctx context.Context, deliveryID string) (*model.Job, error) {
	var id int64
	err := s.db.QueryRowContext(ctx,
		`SELECT id FROM jobs WHERE delivery_id=?`, deliveryID).Scan(&id)
	if err != nil {
		return nil, fmt.Errorf("lookup job by delivery id: %w", err)
	}
	return scanJob(ctx, s.db, id)
}

// scanJob loads a single Job row, resolving repo_id back to owner/name.
func scanJob(ctx context.Context, q querier, id int64) (*model.Job, error) {
	var (
		j           model.Job
		repoID      int64
		event       string
		status      string
		errMsg      sql.NullString
		createdAt   string
		startedAt   sql.NullString
		finishedAt  sql.NullString
		errorType   sql.NullString
		nextAttempt sql.NullString
		retryable   int
	)
	err := q.QueryRowContext(ctx,
		`SELECT id, delivery_id, repo_id, pr_number, event, action, payload,
		        status, attempts, error, COALESCE(error_type,''), COALESCE(retryable,0),
		        created_at, started_at, finished_at, next_attempt_at
		 FROM jobs WHERE id=?`, id).Scan(
		&j.ID, &j.DeliveryID, &repoID, &j.PR.Number, &event, &j.Action, &j.Payload,
		&status, &j.Attempts, &errMsg, &errorType, &retryable, &createdAt, &startedAt, &finishedAt, &nextAttempt)
	if err != nil {
		return nil, fmt.Errorf("scan job: %w", err)
	}
	j.Event = model.EventType(event)
	j.Status = model.JobStatus(status)
	j.Error = errMsg.String
	j.ErrorType = model.ErrorType(errorType.String)
	j.Retryable = retryable != 0
	j.CreatedAt = parseTime(createdAt)
	j.StartedAt = nullableTime(startedAt)
	j.FinishedAt = nullableTime(finishedAt)
	j.NextAttemptAt = nullableTime(nextAttempt)

	// Resolve owner/name from repo_id.
	var owner, name sql.NullString
	if err := q.QueryRowContext(ctx,
		`SELECT owner, name FROM repos WHERE id=?`, repoID).Scan(&owner, &name); err != nil && err != sql.ErrNoRows {
		return nil, fmt.Errorf("resolve job repo: %w", err)
	}
	j.PR.Owner = owner.String
	j.PR.Repo = name.String
	return &j, nil
}
