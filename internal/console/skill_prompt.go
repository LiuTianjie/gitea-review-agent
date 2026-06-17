package console

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"github.com/turning4th/codex-gitea/internal/model"
)

func BuildProjectSkillPrompt(in model.SkillGenerationInput) string {
	evidence := buildAbstractSkillEvidence(in.Context)
	ctxJSON, _ := json.MarshalIndent(evidence, "", "  ")
	existing := "(none)"
	if in.Existing != nil && strings.TrimSpace(in.Existing.Content) != "" {
		existing = in.Existing.Content
	}
	return fmt.Sprintf(`You are evolving a reusable agent SKILL.md for a software project.

Return only the complete SKILL.md content. Do not wrap it in backticks.

Project:
- owner/repo: %s/%s
- purpose: help future coding agents reuse project experience and avoid recurring review mistakes

Current abstracted evidence from review history:
%s

Existing SKILL.md to evolve:
%s

Requirements:
- Produce a valid SKILL.md with YAML frontmatter containing name and description.
- The name must be stable and filesystem-safe: %q.
- The description must say when to use this skill: working in %s/%s or changing related code paths.
- Evolve the existing skill instead of replacing it from scratch. Preserve useful guidance, remove stale duplication, and merge new recurring defect patterns.
- Write a reusable project experience guide, not a review report or issue inventory.
- The evidence is intentionally abstracted. Do not reconstruct, quote, or invent concrete finding titles, file paths, line numbers, PR numbers, commit SHAs, or incident lists.
- Convert evidence signals into general engineering lessons: risk signal, project habit to watch for, what the agent should check, and how to validate.
- Keep examples generic and pattern-shaped, such as "when changing request orchestration..." or "when touching generated assets...". Do not use exact paths, exact lines, exact historical titles, or per-finding summaries.
- If the existing skill is too concrete, rewrite it into broader experience guidelines instead of preserving the concrete wording.
- Keep the skill concise. Prefer project-specific guardrails, checklists, and validation commands over generic review advice.
- Include a section for accumulated project lessons or review heuristics.
- Include a section for validation before handoff.
- Do not claim facts that are not supported by the evidence.
`, in.Context.Owner, in.Context.Repo, string(ctxJSON), existing, in.Context.Slug, in.Context.Owner, in.Context.Repo)
}

type abstractSkillEvidence struct {
	Owner            string                `json:"owner"`
	Repo             string                `json:"repo"`
	Slug             string                `json:"slug"`
	PullRequests     int                   `json:"pull_requests"`
	ReviewRuns       int                   `json:"review_runs"`
	Findings         int                   `json:"findings"`
	OpenFindings     int                   `json:"open_findings"`
	HighCriticalOpen int                   `json:"high_critical_open"`
	TopTags          []model.TagSummary    `json:"top_tags"`
	Signals          []abstractSkillSignal `json:"signals"`
}

type abstractSkillSignal struct {
	Severity  model.Severity `json:"severity"`
	Status    string         `json:"status"`
	Count     int            `json:"count"`
	OpenCount int            `json:"open_count"`
	Tags      []string       `json:"tags,omitempty"`
}

func buildAbstractSkillEvidence(ctx model.ProjectSkillContext) abstractSkillEvidence {
	out := abstractSkillEvidence{
		Owner:            ctx.Owner,
		Repo:             ctx.Repo,
		Slug:             ctx.Slug,
		PullRequests:     ctx.PullRequests,
		ReviewRuns:       ctx.ReviewRuns,
		Findings:         ctx.Findings,
		OpenFindings:     ctx.OpenFindings,
		HighCriticalOpen: ctx.HighCriticalOpen,
		TopTags:          ctx.TopTags,
	}
	byKey := map[string]*abstractSkillSignal{}
	for _, pattern := range ctx.Patterns {
		tags := stableTags(pattern.Tags, 4)
		key := string(pattern.Severity) + "\x00" + pattern.Status + "\x00" + strings.Join(tags, ",")
		if byKey[key] == nil {
			byKey[key] = &abstractSkillSignal{
				Severity: pattern.Severity,
				Status:   pattern.Status,
				Tags:     tags,
			}
		}
		byKey[key].Count += pattern.Count
		byKey[key].OpenCount += pattern.OpenCount
	}
	for _, signal := range byKey {
		out.Signals = append(out.Signals, *signal)
	}
	sort.Slice(out.Signals, func(i, j int) bool {
		a, b := out.Signals[i], out.Signals[j]
		if a.Count != b.Count {
			return a.Count > b.Count
		}
		if a.OpenCount != b.OpenCount {
			return a.OpenCount > b.OpenCount
		}
		if a.Severity != b.Severity {
			return a.Severity < b.Severity
		}
		if a.Status != b.Status {
			return a.Status < b.Status
		}
		return strings.Join(a.Tags, ",") < strings.Join(b.Tags, ",")
	})
	if len(out.Signals) > 12 {
		out.Signals = out.Signals[:12]
	}
	return out
}

func stableTags(tags []string, limit int) []string {
	seen := map[string]bool{}
	var out []string
	for _, tag := range tags {
		tag = strings.ToLower(strings.TrimSpace(tag))
		if tag == "" || seen[tag] {
			continue
		}
		seen[tag] = true
		out = append(out, tag)
	}
	sort.Strings(out)
	if limit > 0 && len(out) > limit {
		out = out[:limit]
	}
	return out
}
