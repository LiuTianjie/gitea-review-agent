package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/turning4th/codex-gitea/internal/model"
)

// CreateReviewRun inserts a running review-run record.
func (s *Store) CreateReviewRun(ctx context.Context, run *model.ReviewRun) error {
	if run == nil {
		return fmt.Errorf("create review run: nil run")
	}
	agent := normalizeAgent(run.Agent)
	now := nowRFC3339()
	jobID := sql.NullInt64{Int64: run.JobID, Valid: run.JobID != 0}
	res, err := s.db.ExecContext(ctx,
		`INSERT INTO review_runs(pull_id,job_id,agent,head_sha,status,error,finding_count,started_at)
		 VALUES(?,?,?,?,?,?,?,?)`,
		run.PullID, jobID, agent, run.HeadSHA, string(model.ReviewRunRunning), "", 0, now)
	if err != nil {
		return fmt.Errorf("create review run: %w", err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		return fmt.Errorf("review run id: %w", err)
	}
	run.ID = id
	run.Agent = agent
	run.Status = model.ReviewRunRunning
	run.StartedAt = parseTime(now)
	return nil
}

// FinishReviewRun records the terminal state of a review run.
func (s *Store) FinishReviewRun(ctx context.Context, runID int64, status model.ReviewRunStatus, errMsg string, findingCount int) error {
	_, err := s.db.ExecContext(ctx,
		`UPDATE review_runs SET status=?, error=?, finding_count=?, finished_at=? WHERE id=?`,
		string(status), errMsg, findingCount, nowRFC3339(), runID)
	if err != nil {
		return fmt.Errorf("finish review run: %w", err)
	}
	return nil
}

// SaveFindings upserts findings for a pull by (pull_id, fingerprint). New rows
// get status='open' and first_seen_sha=headSHA; existing rows keep their
// first_seen_sha and posting metadata but refresh path/line/side/severity.
func (s *Store) SaveFindings(ctx context.Context, pullID int64, headSHA string, fs []model.Finding, opts model.FindingSaveOptions) error {
	if len(fs) == 0 {
		return nil
	}
	agent := normalizeAgent(opts.Agent)
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin save findings tx: %w", err)
	}
	defer tx.Rollback()

	for _, f := range fs {
		fp := effectiveFindingFingerprint(agent, f.Fingerprint())
		tags := normalizeTags(f.Tags)
		tagJSON, _ := json.Marshal(tags)
		reviewRunID := sql.NullInt64{Int64: opts.ReviewRunID, Valid: opts.ReviewRunID != 0}
		mapped := 0
		if opts.MappedFingerprints[fp] || opts.MappedFingerprints[f.Fingerprint()] {
			mapped = 1
		}
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO findings(pull_id,review_run_id,agent,fingerprint,path,line,side,severity,title,body,first_seen_sha,last_seen_sha,status,mapped_inline,tags)
			 VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)
			 ON CONFLICT(pull_id,fingerprint) DO UPDATE SET
			   review_run_id=excluded.review_run_id,
			   agent=excluded.agent,
			   path=excluded.path,
			   line=excluded.line,
			   side=excluded.side,
			   severity=excluded.severity,
			   title=excluded.title,
			   body=excluded.body,
			   last_seen_sha=excluded.last_seen_sha,
			   status='open',
			   mapped_inline=excluded.mapped_inline,
			   tags=excluded.tags`,
			pullID, reviewRunID, agent, fp, f.Path, f.Line, string(f.Side), string(f.Severity),
			f.Title, f.Body, headSHA, headSHA, "open", mapped, string(tagJSON)); err != nil {
			return fmt.Errorf("upsert finding: %w", err)
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit save findings: %w", err)
	}
	return nil
}

func effectiveFindingFingerprint(agent, fp string) string {
	agent = normalizeAgent(agent)
	fp = strings.TrimSpace(fp)
	if strings.HasPrefix(fp, agent+":") {
		return fp
	}
	if fp == "" {
		return agent + ":"
	}
	return agent + ":" + fp
}

func normalizeTags(tags []string) []string {
	const maxTags = 6
	out := make([]string, 0, len(tags))
	seen := map[string]bool{}
	for _, tag := range tags {
		tag = strings.TrimSpace(tag)
		if tag == "" || seen[tag] {
			continue
		}
		seen[tag] = true
		out = append(out, tag)
		if len(out) >= maxTags {
			break
		}
	}
	return out
}

// MarkFindingsFixed marks explicitly resolved prior findings as fixed.
func (s *Store) MarkFindingsFixed(ctx context.Context, pullID int64, fingerprints []string) error {
	if len(fingerprints) == 0 {
		return nil
	}
	placeholders := make([]string, 0, len(fingerprints))
	args := make([]any, 0, len(fingerprints)+1)
	args = append(args, pullID)
	for _, fp := range fingerprints {
		fp = strings.TrimSpace(fp)
		if fp == "" {
			continue
		}
		placeholders = append(placeholders, "?")
		args = append(args, fp)
	}
	if len(placeholders) == 0 {
		return nil
	}
	_, err := s.db.ExecContext(ctx,
		`UPDATE findings SET status='fixed' WHERE pull_id=? AND fingerprint IN (`+strings.Join(placeholders, ",")+`)`,
		args...)
	if err != nil {
		return fmt.Errorf("mark findings fixed: %w", err)
	}
	return nil
}

// ListFindings returns all stored findings for a pull, oldest first.
func (s *Store) ListFindings(ctx context.Context, pullID int64) ([]model.StoredFinding, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT id, pull_id, COALESCE(review_run_id,0), COALESCE(agent,'codex'), fingerprint,
		        path, line, side, severity, title, body,
		        gitea_comment_id, review_id, first_seen_sha, COALESCE(last_seen_sha,''), status,
		        COALESCE(mapped_inline,0), COALESCE(tags,'')
		 FROM findings WHERE pull_id=? ORDER BY id`, pullID)
	if err != nil {
		return nil, fmt.Errorf("list findings: %w", err)
	}
	defer rows.Close()

	var out []model.StoredFinding
	for rows.Next() {
		var (
			sf        model.StoredFinding
			line      sql.NullInt64
			side      sql.NullString
			severity  sql.NullString
			title     sql.NullString
			body      sql.NullString
			commentID sql.NullInt64
			reviewID  sql.NullInt64
			firstSeen sql.NullString
			lastSeen  sql.NullString
			status    sql.NullString
			tags      sql.NullString
			mapped    int
		)
		if err := rows.Scan(&sf.ID, &sf.PullID, &sf.ReviewRunID, &sf.Agent, &sf.Fingerprint, &sf.Path, &line,
			&side, &severity, &title, &body, &commentID, &reviewID, &firstSeen, &lastSeen, &status,
			&mapped, &tags); err != nil {
			return nil, fmt.Errorf("scan finding: %w", err)
		}
		sf.Line = int(line.Int64)
		sf.Side = model.Side(side.String)
		sf.Severity = model.Severity(severity.String)
		sf.Title = title.String
		sf.Body = body.String
		sf.GiteaCommentID = commentID.Int64
		sf.ReviewID = reviewID.Int64
		sf.FirstSeenSHA = firstSeen.String
		sf.LastSeenSHA = lastSeen.String
		sf.MappedInline = mapped != 0
		sf.Status = status.String
		if tags.String != "" {
			_ = json.Unmarshal([]byte(tags.String), &sf.Tags)
		}
		out = append(out, sf)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate findings: %w", err)
	}
	return out, nil
}

// MarkFindingPosted records the Gitea comment id and review id after a finding
// has been posted as an inline review comment.
func (s *Store) MarkFindingPosted(ctx context.Context, findingID, commentID, reviewID int64) error {
	_, err := s.db.ExecContext(ctx,
		`UPDATE findings SET gitea_comment_id=?, review_id=? WHERE id=?`,
		commentID, reviewID, findingID)
	if err != nil {
		return fmt.Errorf("mark finding posted: %w", err)
	}
	return nil
}
