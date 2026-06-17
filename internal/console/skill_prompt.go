package console

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/turning4th/codex-gitea/internal/model"
)

func BuildProjectSkillPrompt(in model.SkillGenerationInput) string {
	ctxJSON, _ := json.MarshalIndent(in.Context, "", "  ")
	existing := "(none)"
	if in.Existing != nil && strings.TrimSpace(in.Existing.Content) != "" {
		existing = in.Existing.Content
	}
	return fmt.Sprintf(`You are evolving a Codex Skill for a software project.

Return only the complete SKILL.md content. Do not wrap it in backticks.

Project:
- owner/repo: %s/%s
- purpose: help future coding agents avoid recurring review defects in this project

Current evidence from review history:
%s

Existing SKILL.md to evolve:
%s

Requirements:
- Produce a valid Codex skill with YAML frontmatter containing name and description.
- The name must be stable and filesystem-safe: %q.
- The description must say when to use this skill: working in %s/%s or changing related code paths.
- Evolve the existing skill instead of replacing it from scratch. Preserve useful guidance, remove stale duplication, and merge new recurring defect patterns.
- Keep the skill concise. Prefer project-specific guardrails, checklists, and validation commands over generic review advice.
- Include a section for recurring defect patterns with evidence-driven guidance.
- Include a section for validation before handoff.
- Do not claim facts that are not supported by the evidence.
`, in.Context.Owner, in.Context.Repo, string(ctxJSON), existing, in.Context.Slug, in.Context.Owner, in.Context.Repo)
}
