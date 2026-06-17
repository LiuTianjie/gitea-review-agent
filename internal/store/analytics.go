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

func (s *Store) BuildAnalysisTrend(ctx context.Context, limit int) ([]model.AnalysisTrendPoint, error) {
	if limit <= 0 || limit > 100 {
		limit = 12
	}
	rows, err := s.db.QueryContext(ctx,
		`SELECT COALESCE(finished_at, started_at)
		 FROM review_runs
		 WHERE status IN (?, ?)
		   AND COALESCE(finished_at, started_at, '') <> ''
		 ORDER BY COALESCE(finished_at, started_at) DESC, id DESC
		 LIMIT ?`, string(model.ReviewRunDone), string(model.ReviewRunFailed), limit)
	if err != nil {
		return nil, fmt.Errorf("analysis trend cutoffs: %w", err)
	}
	defer rows.Close()
	var cutoffs []string
	for rows.Next() {
		var cutoff string
		if err := rows.Scan(&cutoff); err != nil {
			return nil, fmt.Errorf("scan analysis trend cutoff: %w", err)
		}
		cutoffs = append(cutoffs, cutoff)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate analysis trend cutoffs: %w", err)
	}
	sort.Slice(cutoffs, func(i, j int) bool { return cutoffs[i] < cutoffs[j] })

	out := make([]model.AnalysisTrendPoint, 0, len(cutoffs))
	for _, cutoff := range cutoffs {
		point, err := s.analysisTrendPoint(ctx, cutoff)
		if err != nil {
			return nil, err
		}
		out = append(out, point)
	}
	return out, nil
}

func (s *Store) analysisTrendPoint(ctx context.Context, cutoff string) (model.AnalysisTrendPoint, error) {
	var point model.AnalysisTrendPoint
	point.FinishedAt = parseTime(cutoff)
	row := s.db.QueryRowContext(ctx,
		`SELECT
		   COUNT(*),
		   COALESCE(SUM(CASE WHEN status=? THEN 1 ELSE 0 END), 0),
		   COALESCE(SUM(CASE WHEN status=? THEN 1 ELSE 0 END), 0)
		 FROM review_runs
		 WHERE status IN (?, ?)
		   AND COALESCE(finished_at, started_at, '') <> ''
		   AND COALESCE(finished_at, started_at) <= ?`,
		string(model.ReviewRunDone), string(model.ReviewRunFailed),
		string(model.ReviewRunDone), string(model.ReviewRunFailed), cutoff)
	if err := row.Scan(&point.TotalReviewRuns, &point.SuccessfulReviewRuns, &point.FailedReviewRuns); err != nil {
		return model.AnalysisTrendPoint{}, fmt.Errorf("scan analysis trend runs: %w", err)
	}
	completed := point.SuccessfulReviewRuns + point.FailedReviewRuns
	if completed > 0 {
		point.SuccessRate = float64(point.SuccessfulReviewRuns) / float64(completed)
	}

	row = s.db.QueryRowContext(ctx,
		`SELECT
		   COUNT(f.id),
		   COALESCE(SUM(CASE WHEN COALESCE(f.status,'open')='open' THEN 1 ELSE 0 END), 0),
		   COALESCE(SUM(CASE WHEN COALESCE(f.status,'open')='open'
		             AND COALESCE(f.severity,'info') IN (?, ?) THEN 1 ELSE 0 END), 0)
		 FROM findings f
		 JOIN review_runs rr ON rr.id=f.review_run_id
		 WHERE COALESCE(rr.finished_at, rr.started_at, '') <> ''
		   AND COALESCE(rr.finished_at, rr.started_at) <= ?`,
		string(model.SeverityHigh), string(model.SeverityCritical), cutoff)
	if err := row.Scan(&point.TotalFindings, &point.OpenFindings, &point.HighCriticalOpen); err != nil {
		return model.AnalysisTrendPoint{}, fmt.Errorf("scan analysis trend findings: %w", err)
	}
	return point, nil
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
	if err := s.fillDeveloperSummary(ctx, &summary); err != nil {
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
		`SELECT COALESCE(f.agent,'codex'), COALESCE(f.fingerprint,''), COALESCE(f.path,''), COALESCE(f.line,0),
		        COALESCE(f.severity,'info'), COALESCE(f.title,''),
		        COALESCE(f.status,'open'), COALESCE(f.last_seen_sha,''), COALESCE(f.tags,''),
		        COALESCE(r.owner,''), COALESCE(r.name,''), COALESCE(p.number,0), COALESCE(f.pull_id,0)
		 FROM findings f
		 LEFT JOIN pulls p ON p.id=f.pull_id
		 LEFT JOIN repos r ON r.id=p.repo_id
		 ORDER BY f.id DESC`)
	if err != nil {
		return fmt.Errorf("finding summary: %w", err)
	}
	defer rows.Close()

	tagCounts := map[string]int{}
	titleCounts := map[string]int{}
	type overlapKey struct {
		pullID      int64
		fingerprint string
	}
	overlap := map[overlapKey]*model.AgentOverlapSummary{}
	overlapAgents := map[overlapKey]map[string]bool{}

	for rows.Next() {
		var agent, fp, path, severity, title, status, lastSeen, tagsRaw, owner, repo string
		var line, pullNumber int
		var pullID int64
		if err := rows.Scan(&agent, &fp, &path, &line, &severity, &title, &status, &lastSeen, &tagsRaw, &owner, &repo, &pullNumber, &pullID); err != nil {
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
				Agent: agent, Owner: owner, Repo: repo, PullNumber: pullNumber,
				Severity: model.Severity(severity), Title: title,
				Path: path, Line: line, Status: status, LastSeenSHA: lastSeen,
			})
		}
		baseFP := strings.TrimSpace(strings.TrimPrefix(fp, agent+":"))
		if baseFP == "" || pullID == 0 {
			continue
		}
		key := overlapKey{pullID: pullID, fingerprint: baseFP}
		if overlap[key] == nil {
			overlap[key] = &model.AgentOverlapSummary{
				Fingerprint: baseFP, Owner: owner, Repo: repo, PullNumber: pullNumber,
				Title: title, Path: path, Line: line, LastSeenSHA: lastSeen,
			}
			overlapAgents[key] = map[string]bool{}
		}
		overlapAgents[key][agent] = true
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("iterate finding summary: %w", err)
	}
	summary.TopTags = topTagSummaries(tagCounts, 12)
	summary.RepeatedTitles = topTitleSummaries(titleCounts, 12)
	for key, item := range overlap {
		if len(overlapAgents[key]) < 2 {
			continue
		}
		for agent := range overlapAgents[key] {
			item.Agents = append(item.Agents, agent)
		}
		sort.Strings(item.Agents)
		item.AgentCount = len(item.Agents)
		summary.AgentOverlap = append(summary.AgentOverlap, *item)
	}
	sort.Slice(summary.AgentOverlap, func(i, j int) bool {
		a, b := summary.AgentOverlap[i], summary.AgentOverlap[j]
		if a.AgentCount != b.AgentCount {
			return a.AgentCount > b.AgentCount
		}
		if a.Owner != b.Owner {
			return a.Owner < b.Owner
		}
		if a.Repo != b.Repo {
			return a.Repo < b.Repo
		}
		if a.PullNumber != b.PullNumber {
			return a.PullNumber > b.PullNumber
		}
		if a.Path != b.Path {
			return a.Path < b.Path
		}
		if a.Line != b.Line {
			return a.Line < b.Line
		}
		return a.Title < b.Title
	})
	if len(summary.AgentOverlap) > 20 {
		summary.AgentOverlap = summary.AgentOverlap[:20]
	}
	return nil
}

func (s *Store) fillDeveloperSummary(ctx context.Context, summary *model.AnalysisSummary) error {
	items := map[string]*model.DeveloperSummary{}
	get := func(name string) *model.DeveloperSummary {
		name = normalizeDeveloper(name)
		if items[name] == nil {
			items[name] = &model.DeveloperSummary{Developer: name}
		}
		return items[name]
	}

	rows, err := s.db.QueryContext(ctx,
		`SELECT COALESCE(NULLIF(TRIM(author),''),'unknown'), COUNT(*)
		 FROM pulls GROUP BY COALESCE(NULLIF(TRIM(author),''),'unknown')`)
	if err != nil {
		return fmt.Errorf("developer pulls summary: %w", err)
	}
	for rows.Next() {
		var developer string
		var n int
		if err := rows.Scan(&developer, &n); err != nil {
			rows.Close()
			return fmt.Errorf("scan developer pulls summary: %w", err)
		}
		get(developer).PullRequests += n
	}
	if err := rows.Close(); err != nil {
		return fmt.Errorf("close developer pulls summary: %w", err)
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("iterate developer pulls summary: %w", err)
	}

	rows, err = s.db.QueryContext(ctx,
		`SELECT COALESCE(NULLIF(TRIM(p.author),''),'unknown'), rr.status, COUNT(*)
		 FROM review_runs rr
		 JOIN pulls p ON p.id=rr.pull_id
		 GROUP BY COALESCE(NULLIF(TRIM(p.author),''),'unknown'), rr.status`)
	if err != nil {
		return fmt.Errorf("developer review runs summary: %w", err)
	}
	for rows.Next() {
		var developer, status string
		var n int
		if err := rows.Scan(&developer, &status, &n); err != nil {
			rows.Close()
			return fmt.Errorf("scan developer review runs summary: %w", err)
		}
		item := get(developer)
		item.ReviewRuns += n
		switch model.ReviewRunStatus(status) {
		case model.ReviewRunDone:
			item.SuccessfulReviewRuns += n
		case model.ReviewRunFailed:
			item.FailedReviewRuns += n
		}
	}
	if err := rows.Close(); err != nil {
		return fmt.Errorf("close developer review runs summary: %w", err)
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("iterate developer review runs summary: %w", err)
	}

	rows, err = s.db.QueryContext(ctx,
		`SELECT COALESCE(NULLIF(TRIM(p.author),''),'unknown'), COALESCE(f.status,'open'), COALESCE(f.severity,'info'), COUNT(*)
		 FROM findings f
		 JOIN pulls p ON p.id=f.pull_id
		 GROUP BY COALESCE(NULLIF(TRIM(p.author),''),'unknown'), COALESCE(f.status,'open'), COALESCE(f.severity,'info')`)
	if err != nil {
		return fmt.Errorf("developer findings summary: %w", err)
	}
	for rows.Next() {
		var developer, status, severity string
		var n int
		if err := rows.Scan(&developer, &status, &severity, &n); err != nil {
			rows.Close()
			return fmt.Errorf("scan developer findings summary: %w", err)
		}
		item := get(developer)
		item.Findings += n
		if status == "open" {
			item.OpenFindings += n
			if severity == string(model.SeverityHigh) || severity == string(model.SeverityCritical) {
				item.HighCriticalOpen += n
			}
		}
	}
	if err := rows.Close(); err != nil {
		return fmt.Errorf("close developer findings summary: %w", err)
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("iterate developer findings summary: %w", err)
	}

	summary.ByDeveloper = make([]model.DeveloperSummary, 0, len(items))
	for _, item := range items {
		summary.ByDeveloper = append(summary.ByDeveloper, *item)
	}
	sort.Slice(summary.ByDeveloper, func(i, j int) bool {
		a, b := summary.ByDeveloper[i], summary.ByDeveloper[j]
		if a.Findings != b.Findings {
			return a.Findings > b.Findings
		}
		if a.OpenFindings != b.OpenFindings {
			return a.OpenFindings > b.OpenFindings
		}
		if a.FailedReviewRuns != b.FailedReviewRuns {
			return a.FailedReviewRuns > b.FailedReviewRuns
		}
		if a.PullRequests != b.PullRequests {
			return a.PullRequests > b.PullRequests
		}
		return a.Developer < b.Developer
	})
	if len(summary.ByDeveloper) > 20 {
		summary.ByDeveloper = summary.ByDeveloper[:20]
	}
	return nil
}

func normalizeDeveloper(name string) string {
	name = strings.TrimSpace(name)
	if name == "" {
		return "unknown"
	}
	return name
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
