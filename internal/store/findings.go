package store

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"fmt"

	"github.com/turning4th/codex-gitea/internal/model"
)

// fingerprint computes the stable dedup key for a finding (sha256 hex of its
// FingerprintInput).
func fingerprint(f model.Finding) string {
	sum := sha256.Sum256([]byte(f.FingerprintInput()))
	return hex.EncodeToString(sum[:])
}

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
		fp := fingerprint(f)
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO findings(pull_id,fingerprint,path,line,side,severity,first_seen_sha,status)
			 VALUES(?,?,?,?,?,?,?,'open')
			 ON CONFLICT(pull_id,fingerprint) DO UPDATE SET
			   path=excluded.path,
			   line=excluded.line,
			   side=excluded.side,
			   severity=excluded.severity`,
			pullID, fp, f.Path, f.Line, string(f.Side), string(f.Severity), headSHA); err != nil {
			return fmt.Errorf("upsert finding: %w", err)
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit save findings: %w", err)
	}
	return nil
}

// ListFindings returns all stored findings for a pull, oldest first.
func (s *Store) ListFindings(ctx context.Context, pullID int64) ([]model.StoredFinding, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT id, pull_id, fingerprint, path, line, side, severity,
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
			commentID sql.NullInt64
			reviewID  sql.NullInt64
			firstSeen sql.NullString
			status    sql.NullString
		)
		if err := rows.Scan(&sf.ID, &sf.PullID, &sf.Fingerprint, &sf.Path, &line,
			&side, &severity, &commentID, &reviewID, &firstSeen, &status); err != nil {
			return nil, fmt.Errorf("scan finding: %w", err)
		}
		sf.Line = int(line.Int64)
		sf.Side = model.Side(side.String)
		sf.Severity = model.Severity(severity.String)
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
