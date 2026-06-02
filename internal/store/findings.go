package store

import (
	"context"
	"database/sql"
	"fmt"
	"strings"

	"github.com/turning4th/codex-gitea/internal/model"
)

// SaveFindings upserts findings for a pull by (pull_id, fingerprint). New rows
// get status='open' and first_seen_sha=headSHA; existing rows keep their
// first_seen_sha and posting metadata but refresh path/line/side/severity.
func (s *Store) SaveFindings(ctx context.Context, pullID int64, headSHA string, fs []model.Finding) error {
	if len(fs) == 0 {
		return nil
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin save findings tx: %w", err)
	}
	defer tx.Rollback()

	for _, f := range fs {
		fp := f.Fingerprint()
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO findings(pull_id,fingerprint,path,line,side,severity,title,body,first_seen_sha,status)
			 VALUES(?,?,?,?,?,?,?,?,?,'open')
			 ON CONFLICT(pull_id,fingerprint) DO UPDATE SET
			   path=excluded.path,
			   line=excluded.line,
			   side=excluded.side,
			   severity=excluded.severity,
			   title=excluded.title,
			   body=excluded.body,
			   status='open'`,
			pullID, fp, f.Path, f.Line, string(f.Side), string(f.Severity), f.Title, f.Body, headSHA); err != nil {
			return fmt.Errorf("upsert finding: %w", err)
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit save findings: %w", err)
	}
	return nil
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
		`SELECT id, pull_id, fingerprint, path, line, side, severity, title, body,
		        gitea_comment_id, review_id, first_seen_sha, status
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
			status    sql.NullString
		)
		if err := rows.Scan(&sf.ID, &sf.PullID, &sf.Fingerprint, &sf.Path, &line,
			&side, &severity, &title, &body, &commentID, &reviewID, &firstSeen, &status); err != nil {
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
		sf.Status = status.String
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
