package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"github.com/turning4th/codex-gitea/internal/model"
)

func (s *Store) CreateAnalysisReport(ctx context.Context, summary model.AnalysisSummary) (*model.AnalysisReport, error) {
	data, err := json.Marshal(summary)
	if err != nil {
		return nil, fmt.Errorf("marshal analysis summary: %w", err)
	}
	now := nowRFC3339()
	res, err := s.db.ExecContext(ctx,
		`INSERT INTO analysis_reports(summary_json,created_at) VALUES(?,?)`, string(data), now)
	if err != nil {
		return nil, fmt.Errorf("insert analysis report: %w", err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		return nil, fmt.Errorf("analysis report id: %w", err)
	}
	return &model.AnalysisReport{ID: id, CreatedAt: parseTime(now), Summary: summary}, nil
}

func (s *Store) LatestAnalysisReport(ctx context.Context) (*model.AnalysisReport, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT id, summary_json, created_at FROM analysis_reports ORDER BY id DESC LIMIT 1`)
	if err != nil {
		return nil, fmt.Errorf("latest analysis report: %w", err)
	}
	defer rows.Close()
	reports, err := scanAnalysisReports(rows)
	if err != nil {
		return nil, err
	}
	if len(reports) == 0 {
		return nil, nil
	}
	return &reports[0], nil
}

func (s *Store) ListAnalysisReports(ctx context.Context, limit int) ([]model.AnalysisReport, error) {
	if limit <= 0 || limit > 100 {
		limit = 20
	}
	rows, err := s.db.QueryContext(ctx,
		`SELECT id, summary_json, created_at FROM analysis_reports ORDER BY id DESC LIMIT ?`, limit)
	if err != nil {
		return nil, fmt.Errorf("list analysis reports: %w", err)
	}
	defer rows.Close()
	return scanAnalysisReports(rows)
}

func scanAnalysisReports(rows *sql.Rows) ([]model.AnalysisReport, error) {
	var out []model.AnalysisReport
	for rows.Next() {
		var (
			r       model.AnalysisReport
			payload string
			created string
		)
		if err := rows.Scan(&r.ID, &payload, &created); err != nil {
			return nil, fmt.Errorf("scan analysis report: %w", err)
		}
		if err := json.Unmarshal([]byte(payload), &r.Summary); err != nil {
			return nil, fmt.Errorf("parse analysis report: %w", err)
		}
		r.CreatedAt = parseTime(created)
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate analysis reports: %w", err)
	}
	return out, nil
}

func (s *Store) BuildAnalysisSummary(ctx context.Context) (model.AnalysisSummary, error) {
	summary := model.AnalysisSummary{
		ByAgent:    map[string]model.AgentSummary{},
		BySeverity: map[string]int{},
		ByStatus:   map[string]int{},
	}
	if err := s.fillReviewRunSummary(ctx, &summary); err != nil {
		return model.AnalysisSummary{}, err
	}
	if err := s.fillFindingSummary(ctx, &summary); err != nil {
		return model.AnalysisSummary{}, err
	}
	completed := summary.SuccessfulReviewRuns + summary.FailedReviewRuns
	if completed > 0 {
		summary.SuccessRate = float64(summary.SuccessfulReviewRuns) / float64(completed)
	}
	return summary, nil
}

func (s *Store) fillReviewRunSummary(ctx context.Context, summary *model.AnalysisSummary) error {
	rows, err := s.db.QueryContext(ctx,
		`SELECT COALESCE(agent,'codex'), status, COUNT(*)
		 FROM review_runs GROUP BY COALESCE(agent,'codex'), status`)
	if err != nil {
		return fmt.Errorf("review run summary: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var agent, status string
		var n int
		if err := rows.Scan(&agent, &status, &n); err != nil {
			return fmt.Errorf("scan review run summary: %w", err)
		}
		as := summary.ByAgent[agent]
		as.ReviewRuns += n
		switch model.ReviewRunStatus(status) {
		case model.ReviewRunDone:
			as.Succeeded += n
			summary.SuccessfulReviewRuns += n
		case model.ReviewRunFailed:
			as.Failed += n
			summary.FailedReviewRuns += n
		}
		summary.TotalReviewRuns += n
		summary.ByAgent[agent] = as
	}
	return rows.Err()
}

func (s *Store) fillFindingSummary(ctx context.Context, summary *model.AnalysisSummary) error {
	rows, err := s.db.QueryContext(ctx,
		`SELECT COALESCE(agent,'codex'), COALESCE(fingerprint,''), COALESCE(path,''), COALESCE(line,0),
		        COALESCE(severity,'info'), COALESCE(title,''),
		        COALESCE(status,'open'), COALESCE(last_seen_sha,''), COALESCE(tags,'')
		 FROM findings ORDER BY id DESC`)
	if err != nil {
		return fmt.Errorf("finding summary: %w", err)
	}
	defer rows.Close()

	tagCounts := map[string]int{}
	titleCounts := map[string]int{}
	overlap := map[string]*model.AgentOverlapSummary{}
	overlapAgents := map[string]map[string]bool{}

	for rows.Next() {
		var agent, fp, path, severity, title, status, lastSeen, tagsRaw string
		var line int
		if err := rows.Scan(&agent, &fp, &path, &line, &severity, &title, &status, &lastSeen, &tagsRaw); err != nil {
			return fmt.Errorf("scan finding summary: %w", err)
		}
		summary.TotalFindings++
		summary.BySeverity[severity]++
		summary.ByStatus[status]++
		as := summary.ByAgent[agent]
		as.Findings++
		if status == "open" {
			as.Open++
			summary.OpenFindings++
			if severity == string(model.SeverityHigh) || severity == string(model.SeverityCritical) {
				summary.HighCriticalOpen++
			}
		} else if status == "fixed" {
			summary.FixedFindings++
		}
		summary.ByAgent[agent] = as

		if title != "" {
			titleCounts[title]++
		}
		var tags []string
		_ = json.Unmarshal([]byte(tagsRaw), &tags)
		for _, tag := range normalizeTags(tags) {
			tagCounts[tag]++
		}
		if (severity == string(model.SeverityHigh) || severity == string(model.SeverityCritical)) && len(summary.RecentSevere) < 10 {
			summary.RecentSevere = append(summary.RecentSevere, model.SevereFindingSummary{
				Agent: agent, Severity: model.Severity(severity), Title: title,
				Path: path, Line: line, Status: status, LastSeenSHA: lastSeen,
			})
		}
		baseFP := strings.TrimPrefix(fp, agent+":")
		if overlap[baseFP] == nil {
			overlap[baseFP] = &model.AgentOverlapSummary{Fingerprint: baseFP, Title: title, Path: path, Line: line}
			overlapAgents[baseFP] = map[string]bool{}
		}
		overlapAgents[baseFP][agent] = true
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("iterate finding summary: %w", err)
	}
	summary.TopTags = topTagSummaries(tagCounts, 12)
	summary.RepeatedTitles = topTitleSummaries(titleCounts, 12)
	for fp, item := range overlap {
		if len(overlapAgents[fp]) < 2 {
			continue
		}
		for agent := range overlapAgents[fp] {
			item.Agents = append(item.Agents, agent)
		}
		sort.Strings(item.Agents)
		summary.AgentOverlap = append(summary.AgentOverlap, *item)
		if len(summary.AgentOverlap) >= 20 {
			break
		}
	}
	return nil
}

func topTagSummaries(counts map[string]int, limit int) []model.TagSummary {
	items := make([]model.TagSummary, 0, len(counts))
	for tag, count := range counts {
		items = append(items, model.TagSummary{Tag: tag, Count: count})
	}
	sort.Slice(items, func(i, j int) bool {
		if items[i].Count == items[j].Count {
			return items[i].Tag < items[j].Tag
		}
		return items[i].Count > items[j].Count
	})
	if len(items) > limit {
		items = items[:limit]
	}
	return items
}

func topTitleSummaries(counts map[string]int, limit int) []model.TitleSummary {
	items := make([]model.TitleSummary, 0, len(counts))
	for title, count := range counts {
		if count < 2 {
			continue
		}
		items = append(items, model.TitleSummary{Title: title, Count: count})
	}
	sort.Slice(items, func(i, j int) bool {
		if items[i].Count == items[j].Count {
			return items[i].Title < items[j].Title
		}
		return items[i].Count > items[j].Count
	})
	if len(items) > limit {
		items = items[:limit]
	}
	return items
}
