package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"

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
// it. Returns (nil, nil) when no pending job exists. The SELECT+UPDATE run in a
// single transaction so concurrent workers never claim the same row.
func (s *Store) ClaimJob(ctx context.Context) (*model.Job, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("begin claim tx: %w", err)
	}
	defer tx.Rollback()

	var id int64
	err = tx.QueryRowContext(ctx,
		`SELECT j.id
		 FROM jobs j
		 WHERE j.status=?
		   AND NOT EXISTS (
		     SELECT 1 FROM jobs r
		     WHERE r.status=?
		       AND r.repo_id=j.repo_id
		       AND r.pr_number=j.pr_number
		   )
		 ORDER BY j.id LIMIT 1`,
		string(model.JobPending), string(model.JobRunning)).Scan(&id)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("select pending job: %w", err)
	}

	if _, err := tx.ExecContext(ctx,
		`UPDATE jobs SET status=?, attempts=attempts+1, started_at=? WHERE id=?`,
		string(model.JobRunning), nowRFC3339(), id); err != nil {
		return nil, fmt.Errorf("claim update: %w", err)
	}

	job, err := scanJob(ctx, tx, id)
	if err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("commit claim: %w", err)
	}
	return job, nil
}

// FinishJob sets a terminal status (done/failed/superseded) and the error msg.
func (s *Store) FinishJob(ctx context.Context, id int64, status model.JobStatus, errMsg string) error {
	_, err := s.db.ExecContext(ctx,
		`UPDATE jobs SET status=?, error=?, finished_at=? WHERE id=?`,
		string(status), errMsg, nowRFC3339(), id)
	if err != nil {
		return fmt.Errorf("finish job: %w", err)
	}
	return nil
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

// ListJobs returns up to limit jobs (newest first) as console read models,
// joined to repo owner/name and the PR's session id.
func (s *Store) ListJobs(ctx context.Context, limit int) ([]model.JobView, error) {
	if limit <= 0 {
		limit = 50
	}
	rows, err := s.db.QueryContext(ctx,
		`SELECT j.id, r.owner, r.name, j.pr_number, j.event, j.action,
		        j.status, j.attempts, j.error, j.created_at, j.started_at, j.finished_at,
		        COALESCE(p.session_id,'')
		 FROM jobs j
		 LEFT JOIN repos r ON r.id = j.repo_id
		 LEFT JOIN pulls p ON p.repo_id = j.repo_id AND p.number = j.pr_number
		 ORDER BY j.id DESC LIMIT ?`, limit)
	if err != nil {
		return nil, fmt.Errorf("list jobs: %w", err)
	}
	defer rows.Close()

	var out []model.JobView
	for rows.Next() {
		var (
			v          model.JobView
			owner      sql.NullString
			name       sql.NullString
			event      string
			status     string
			errMsg     sql.NullString
			createdAt  string
			startedAt  sql.NullString
			finishedAt sql.NullString
			sessionID  string
		)
		if err := rows.Scan(&v.ID, &owner, &name, &v.PR.Number, &event, &v.Action,
			&status, &v.Attempts, &errMsg, &createdAt, &startedAt, &finishedAt, &sessionID); err != nil {
			return nil, fmt.Errorf("scan job view: %w", err)
		}
		v.PR.Owner = owner.String
		v.PR.Repo = name.String
		v.Event = model.EventType(event)
		v.Status = model.JobStatus(status)
		v.Error = errMsg.String
		v.CreatedAt = parseTime(createdAt)
		v.StartedAt = nullableTime(startedAt)
		v.FinishedAt = nullableTime(finishedAt)
		v.SessionID = sessionID
		out = append(out, v)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate job views: %w", err)
	}
	if err := rows.Close(); err != nil {
		return nil, fmt.Errorf("close job rows: %w", err)
	}
	for i := range out {
		logs, err := s.listJobLogs(ctx, out[i].ID)
		if err != nil {
			return nil, err
		}
		out[i].Logs = logs
	}
	return out, nil
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
		j          model.Job
		repoID     int64
		event      string
		status     string
		errMsg     sql.NullString
		createdAt  string
		startedAt  sql.NullString
		finishedAt sql.NullString
	)
	err := q.QueryRowContext(ctx,
		`SELECT id, delivery_id, repo_id, pr_number, event, action, payload,
		        status, attempts, error, created_at, started_at, finished_at
		 FROM jobs WHERE id=?`, id).Scan(
		&j.ID, &j.DeliveryID, &repoID, &j.PR.Number, &event, &j.Action, &j.Payload,
		&status, &j.Attempts, &errMsg, &createdAt, &startedAt, &finishedAt)
	if err != nil {
		return nil, fmt.Errorf("scan job: %w", err)
	}
	j.Event = model.EventType(event)
	j.Status = model.JobStatus(status)
	j.Error = errMsg.String
	j.CreatedAt = parseTime(createdAt)
	j.StartedAt = nullableTime(startedAt)
	j.FinishedAt = nullableTime(finishedAt)

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
