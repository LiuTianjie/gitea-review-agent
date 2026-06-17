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

func (s *Store) ListProjectSkillSummaries(ctx context.Context) ([]model.ProjectSkillSummary, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT r.owner, r.name,
		       COUNT(DISTINCT p.id),
		       COUNT(DISTINCT rr.id),
		       COUNT(DISTINCT f.id),
		       COUNT(DISTINCT CASE WHEN COALESCE(f.status,'open')='open' THEN f.id END),
		       COUNT(DISTINCT CASE WHEN COALESCE(f.status,'open')='open' AND COALESCE(f.severity,'info') IN (?, ?) THEN f.id END),
		       COALESCE(ps.slug,''), COALESCE(ps.version,0), COALESCE(ps.updated_at,'')
		FROM repos r
		JOIN pulls p ON p.repo_id=r.id
		LEFT JOIN review_runs rr ON rr.pull_id=p.id
		LEFT JOIN findings f ON f.pull_id=p.id
		LEFT JOIN project_skills ps ON ps.owner=r.owner AND ps.repo=r.name
		GROUP BY r.owner, r.name, ps.slug, ps.version, ps.updated_at
		HAVING COUNT(DISTINCT f.id) > 0
		ORDER BY COUNT(DISTINCT f.id) DESC, r.owner, r.name`,
		string(model.SeverityHigh), string(model.SeverityCritical))
	if err != nil {
		return nil, fmt.Errorf("list project skill summaries: %w", err)
	}
	defer rows.Close()

	var out []model.ProjectSkillSummary
	for rows.Next() {
		var item model.ProjectSkillSummary
		var updated string
		if err := rows.Scan(&item.Owner, &item.Repo, &item.PullRequests, &item.ReviewRuns, &item.Findings,
			&item.OpenFindings, &item.HighCriticalOpen, &item.Slug, &item.SkillVersion, &updated); err != nil {
			return nil, fmt.Errorf("scan project skill summary: %w", err)
		}
		if item.Slug == "" {
			item.Slug = projectSkillSlug(item.Owner, item.Repo)
		}
		item.SkillUpdatedAt = parseTime(updated)
		out = append(out, item)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate project skill summaries: %w", err)
	}
	return out, nil
}

func (s *Store) GetProjectSkill(ctx context.Context, owner, repo string) (*model.ProjectSkill, error) {
	var skill model.ProjectSkill
	var created, updated sql.NullString
	err := s.db.QueryRowContext(ctx, `
		SELECT id, owner, repo, slug, title, content, version, source_finding_count, created_at, updated_at
		FROM project_skills
		WHERE owner=? AND repo=?`, owner, repo).Scan(
		&skill.ID, &skill.Owner, &skill.Repo, &skill.Slug, &skill.Title, &skill.Content,
		&skill.Version, &skill.SourceFindingCount, &created, &updated)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("get project skill: %w", err)
	}
	skill.CreatedAt = parseTime(created.String)
	skill.UpdatedAt = parseTime(updated.String)
	return &skill, nil
}

func (s *Store) UpsertProjectSkill(ctx context.Context, skill *model.ProjectSkill) error {
	if skill == nil {
		return fmt.Errorf("upsert project skill: nil skill")
	}
	skill.Owner = strings.TrimSpace(skill.Owner)
	skill.Repo = strings.TrimSpace(skill.Repo)
	skill.Content = strings.TrimSpace(skill.Content)
	if skill.Owner == "" || skill.Repo == "" {
		return fmt.Errorf("upsert project skill: empty owner or repo")
	}
	if skill.Content == "" {
		return fmt.Errorf("upsert project skill: empty content")
	}
	if skill.Slug == "" {
		skill.Slug = projectSkillSlug(skill.Owner, skill.Repo)
	}
	if skill.Title == "" {
		skill.Title = skill.Owner + "/" + skill.Repo + " defect-prevention skill"
	}
	now := nowRFC3339()
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO project_skills(owner,repo,slug,title,content,version,source_finding_count,created_at,updated_at)
		VALUES(?,?,?,?,?,?,?,?,?)
		ON CONFLICT(owner,repo) DO UPDATE SET
		  slug=excluded.slug,
		  title=excluded.title,
		  content=excluded.content,
		  version=project_skills.version+1,
		  source_finding_count=excluded.source_finding_count,
		  updated_at=excluded.updated_at`,
		skill.Owner, skill.Repo, skill.Slug, skill.Title, skill.Content, 1, skill.SourceFindingCount, now, now)
	if err != nil {
		return fmt.Errorf("upsert project skill: %w", err)
	}
	return nil
}

func (s *Store) BuildProjectSkillContext(ctx context.Context, owner, repo string) (model.ProjectSkillContext, error) {
	out := model.ProjectSkillContext{Owner: owner, Repo: repo, Slug: projectSkillSlug(owner, repo)}
	row := s.db.QueryRowContext(ctx, `
		SELECT COUNT(DISTINCT p.id),
		       COUNT(DISTINCT rr.id),
		       COUNT(DISTINCT f.id),
		       COUNT(DISTINCT CASE WHEN COALESCE(f.status,'open')='open' THEN f.id END),
		       COUNT(DISTINCT CASE WHEN COALESCE(f.status,'open')='open' AND COALESCE(f.severity,'info') IN (?, ?) THEN f.id END)
		FROM repos r
		JOIN pulls p ON p.repo_id=r.id
		LEFT JOIN review_runs rr ON rr.pull_id=p.id
		LEFT JOIN findings f ON f.pull_id=p.id
		WHERE r.owner=? AND r.name=?`,
		string(model.SeverityHigh), string(model.SeverityCritical), owner, repo)
	if err := row.Scan(&out.PullRequests, &out.ReviewRuns, &out.Findings, &out.OpenFindings, &out.HighCriticalOpen); err != nil {
		return model.ProjectSkillContext{}, fmt.Errorf("project skill context totals: %w", err)
	}
	if out.Findings == 0 {
		return out, nil
	}

	tags, err := s.projectTopTags(ctx, owner, repo)
	if err != nil {
		return model.ProjectSkillContext{}, err
	}
	out.TopTags = tags
	patterns, err := s.projectSkillPatterns(ctx, owner, repo)
	if err != nil {
		return model.ProjectSkillContext{}, err
	}
	out.Patterns = patterns
	return out, nil
}

func (s *Store) projectTopTags(ctx context.Context, owner, repo string) ([]model.TagSummary, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT COALESCE(f.tags,'')
		FROM findings f
		JOIN pulls p ON p.id=f.pull_id
		JOIN repos r ON r.id=p.repo_id
		WHERE r.owner=? AND r.name=?`, owner, repo)
	if err != nil {
		return nil, fmt.Errorf("project skill tags: %w", err)
	}
	defer rows.Close()
	counts := map[string]int{}
	for rows.Next() {
		var raw string
		if err := rows.Scan(&raw); err != nil {
			return nil, fmt.Errorf("scan project skill tags: %w", err)
		}
		var tags []string
		_ = json.Unmarshal([]byte(raw), &tags)
		for _, tag := range normalizeTags(tags) {
			counts[tag]++
		}
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate project skill tags: %w", err)
	}
	return topTagSummaries(counts, 10), nil
}

func (s *Store) projectSkillPatterns(ctx context.Context, owner, repo string) ([]model.ProjectSkillPattern, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT COALESCE(f.title,''), COALESCE(f.severity,'info'), COALESCE(f.status,'open'),
		       COUNT(*),
		       SUM(CASE WHEN COALESCE(f.status,'open')='open' THEN 1 ELSE 0 END),
		       COALESCE(MIN(NULLIF(f.path,'')), ''), COALESCE(MIN(COALESCE(f.line,0)), 0),
		       GROUP_CONCAT(DISTINCT COALESCE(f.agent,'codex')),
		       GROUP_CONCAT(COALESCE(f.tags,''), char(31))
		FROM findings f
		JOIN pulls p ON p.id=f.pull_id
		JOIN repos r ON r.id=p.repo_id
		WHERE r.owner=? AND r.name=?
		GROUP BY COALESCE(f.title,''), COALESCE(f.severity,'info'), COALESCE(f.status,'open')
		ORDER BY COUNT(*) DESC,
		         SUM(CASE WHEN COALESCE(f.status,'open')='open' THEN 1 ELSE 0 END) DESC,
		         COALESCE(f.severity,'info')
		LIMIT 30`, owner, repo)
	if err != nil {
		return nil, fmt.Errorf("project skill patterns: %w", err)
	}
	defer rows.Close()

	var out []model.ProjectSkillPattern
	for rows.Next() {
		var p model.ProjectSkillPattern
		var severity, agentsRaw, tagsRaw string
		if err := rows.Scan(&p.Title, &severity, &p.Status, &p.Count, &p.OpenCount,
			&p.SamplePath, &p.SampleLine, &agentsRaw, &tagsRaw); err != nil {
			return nil, fmt.Errorf("scan project skill pattern: %w", err)
		}
		p.Severity = model.Severity(severity)
		p.Agents = splitDistinctCSV(agentsRaw, 6)
		p.Tags = tagsFromConcat(tagsRaw, 6)
		out = append(out, p)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate project skill patterns: %w", err)
	}
	return out, nil
}

func projectSkillSlug(owner, repo string) string {
	raw := strings.ToLower(strings.TrimSpace(owner + "-" + repo + "-defect-skill"))
	var b strings.Builder
	prevDash := false
	for _, r := range raw {
		ok := (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9')
		if ok {
			b.WriteRune(r)
			prevDash = false
			continue
		}
		if !prevDash {
			b.WriteByte('-')
			prevDash = true
		}
	}
	return strings.Trim(b.String(), "-")
}

func splitDistinctCSV(raw string, limit int) []string {
	parts := strings.Split(raw, ",")
	out := make([]string, 0, len(parts))
	seen := map[string]bool{}
	for _, part := range parts {
		part = strings.TrimSpace(part)
		if part == "" || seen[part] {
			continue
		}
		seen[part] = true
		out = append(out, part)
		if len(out) >= limit {
			break
		}
	}
	sort.Strings(out)
	return out
}

func tagsFromConcat(raw string, limit int) []string {
	counts := map[string]int{}
	for _, item := range strings.Split(raw, "\x1f") {
		var tags []string
		if strings.HasPrefix(strings.TrimSpace(item), "[") {
			_ = json.Unmarshal([]byte(item), &tags)
		}
		for _, tag := range normalizeTags(tags) {
			counts[tag]++
		}
	}
	summaries := topTagSummaries(counts, limit)
	out := make([]string, 0, len(summaries))
	for _, summary := range summaries {
		out = append(out, summary.Tag)
	}
	return out
}
