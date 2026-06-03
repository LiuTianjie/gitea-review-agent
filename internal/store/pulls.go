package store

import (
	"context"
	"database/sql"
	"fmt"
	"strings"

	"github.com/turning4th/codex-gitea/internal/model"
)

const defaultReviewerAgent = "codex"

// GetPull returns the persisted state for pr, or (nil, nil) if absent.
func (s *Store) GetPull(ctx context.Context, pr model.PRRef) (*model.PullState, error) {
	repoID, ok, err := lookupRepoID(ctx, s.db, pr.Owner, pr.Repo)
	if err != nil {
		return nil, fmt.Errorf("get pull lookup repo: %w", err)
	}
	if !ok {
		return nil, nil
	}

	var (
		st           model.PullState
		sessionID    sql.NullString
		headSHA      sql.NullString
		baseRef      sql.NullString
		lastReviewID sql.NullInt64
		updatedAt    sql.NullString
	)
	err = s.db.QueryRowContext(ctx,
		`SELECT id, session_id, head_sha, base_ref, last_review_id, updated_at
		 FROM pulls WHERE repo_id=? AND number=?`, repoID, pr.Number).Scan(
		&st.ID, &sessionID, &headSHA, &baseRef, &lastReviewID, &updatedAt)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("scan pull: %w", err)
	}
	st.PR = pr
	st.SessionID = sessionID.String
	st.HeadSHA = headSHA.String
	st.BaseRef = baseRef.String
	st.LastReviewID = lastReviewID.Int64
	st.UpdatedAt = parseTime(updatedAt.String)
	return &st, nil
}

// UpsertPull inserts or updates the pull row for st.PR, auto-creating the repo
// and pull rows as needed. Fields are overwritten with st's values.
func (s *Store) UpsertPull(ctx context.Context, st *model.PullState) error {
	repoID, err := ensureRepo(ctx, s.db, st.PR.Owner, st.PR.Repo)
	if err != nil {
		return err
	}
	_, err = s.db.ExecContext(ctx,
		`INSERT INTO pulls(repo_id,number,session_id,head_sha,base_ref,last_review_id,updated_at)
		 VALUES(?,?,?,?,?,?,?)
		 ON CONFLICT(repo_id,number) DO UPDATE SET
		   session_id=excluded.session_id,
		   head_sha=excluded.head_sha,
		   base_ref=excluded.base_ref,
		   last_review_id=excluded.last_review_id,
		   updated_at=excluded.updated_at`,
		repoID, st.PR.Number, st.SessionID, st.HeadSHA, st.BaseRef,
		st.LastReviewID, nowRFC3339())
	if err != nil {
		return fmt.Errorf("upsert pull: %w", err)
	}
	if err := s.UpsertReviewerState(ctx, &model.PullReviewerState{
		PR:           st.PR,
		Agent:        defaultReviewerAgent,
		SessionID:    st.SessionID,
		HeadSHA:      st.HeadSHA,
		BaseRef:      st.BaseRef,
		LastReviewID: st.LastReviewID,
	}); err != nil {
		return err
	}
	_ = repoID
	return nil
}

// SetSession sets the codex session id for pr, creating the pull row if needed.
func (s *Store) SetSession(ctx context.Context, pr model.PRRef, sessionID string) error {
	if _, err := ensurePull(ctx, s.db, pr); err != nil {
		return err
	}
	repoID, _, err := lookupRepoID(ctx, s.db, pr.Owner, pr.Repo)
	if err != nil {
		return err
	}
	_, err = s.db.ExecContext(ctx,
		`UPDATE pulls SET session_id=?, updated_at=? WHERE repo_id=? AND number=?`,
		sessionID, nowRFC3339(), repoID, pr.Number)
	if err != nil {
		return fmt.Errorf("set session: %w", err)
	}
	st, err := s.GetReviewerState(ctx, pr, defaultReviewerAgent)
	if err != nil {
		return err
	}
	if st == nil {
		st = &model.PullReviewerState{PR: pr, Agent: defaultReviewerAgent}
	}
	st.SessionID = sessionID
	return s.UpsertReviewerState(ctx, st)
}

// SetLastReview records the last submitted review id for pr, creating the pull
// row if needed.
func (s *Store) SetLastReview(ctx context.Context, pr model.PRRef, reviewID int64) error {
	if _, err := ensurePull(ctx, s.db, pr); err != nil {
		return err
	}
	repoID, _, err := lookupRepoID(ctx, s.db, pr.Owner, pr.Repo)
	if err != nil {
		return err
	}
	_, err = s.db.ExecContext(ctx,
		`UPDATE pulls SET last_review_id=?, updated_at=? WHERE repo_id=? AND number=?`,
		reviewID, nowRFC3339(), repoID, pr.Number)
	if err != nil {
		return fmt.Errorf("set last review: %w", err)
	}
	st, err := s.GetReviewerState(ctx, pr, defaultReviewerAgent)
	if err != nil {
		return err
	}
	if st == nil {
		st = &model.PullReviewerState{PR: pr, Agent: defaultReviewerAgent}
	}
	st.LastReviewID = reviewID
	return s.UpsertReviewerState(ctx, st)
}

func normalizeAgent(agent string) string {
	agent = strings.ToLower(strings.TrimSpace(agent))
	if agent == "" {
		return defaultReviewerAgent
	}
	return agent
}

// GetReviewerState returns the persisted session/review state for one agent.
func (s *Store) GetReviewerState(ctx context.Context, pr model.PRRef, agent string) (*model.PullReviewerState, error) {
	repoID, ok, err := lookupRepoID(ctx, s.db, pr.Owner, pr.Repo)
	if err != nil {
		return nil, fmt.Errorf("get reviewer state lookup repo: %w", err)
	}
	if !ok {
		return nil, nil
	}
	var pullID int64
	err = s.db.QueryRowContext(ctx,
		`SELECT id FROM pulls WHERE repo_id=? AND number=?`, repoID, pr.Number).Scan(&pullID)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("lookup pull: %w", err)
	}

	var (
		st           model.PullReviewerState
		sessionID    sql.NullString
		headSHA      sql.NullString
		baseRef      sql.NullString
		lastReviewID sql.NullInt64
		updatedAt    sql.NullString
	)
	err = s.db.QueryRowContext(ctx,
		`SELECT id, session_id, head_sha, base_ref, last_review_id, updated_at
		 FROM pull_reviewer_states WHERE pull_id=? AND agent=?`,
		pullID, normalizeAgent(agent)).Scan(&st.ID, &sessionID, &headSHA, &baseRef, &lastReviewID, &updatedAt)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("scan reviewer state: %w", err)
	}
	st.PullID = pullID
	st.PR = pr
	st.Agent = normalizeAgent(agent)
	st.SessionID = sessionID.String
	st.HeadSHA = headSHA.String
	st.BaseRef = baseRef.String
	st.LastReviewID = lastReviewID.Int64
	st.UpdatedAt = parseTime(updatedAt.String)
	return &st, nil
}

// UpsertReviewerState inserts or updates per-agent PR review state.
func (s *Store) UpsertReviewerState(ctx context.Context, st *model.PullReviewerState) error {
	pullID, err := ensurePull(ctx, s.db, st.PR)
	if err != nil {
		return err
	}
	agent := normalizeAgent(st.Agent)
	_, err = s.db.ExecContext(ctx,
		`INSERT INTO pull_reviewer_states(pull_id,agent,session_id,head_sha,base_ref,last_review_id,updated_at)
		 VALUES(?,?,?,?,?,?,?)
		 ON CONFLICT(pull_id,agent) DO UPDATE SET
		   session_id=excluded.session_id,
		   head_sha=excluded.head_sha,
		   base_ref=excluded.base_ref,
		   last_review_id=excluded.last_review_id,
		   updated_at=excluded.updated_at`,
		pullID, agent, st.SessionID, st.HeadSHA, st.BaseRef, st.LastReviewID, nowRFC3339())
	if err != nil {
		return fmt.Errorf("upsert reviewer state: %w", err)
	}
	if agent == defaultReviewerAgent {
		repoID, _, rerr := lookupRepoID(ctx, s.db, st.PR.Owner, st.PR.Repo)
		if rerr != nil {
			return rerr
		}
		if _, err := s.db.ExecContext(ctx,
			`UPDATE pulls SET session_id=?, head_sha=?, base_ref=?, last_review_id=?, updated_at=?
			 WHERE repo_id=? AND number=?`,
			st.SessionID, st.HeadSHA, st.BaseRef, st.LastReviewID, nowRFC3339(), repoID, st.PR.Number); err != nil {
			return fmt.Errorf("sync codex pull state: %w", err)
		}
	}
	return nil
}
