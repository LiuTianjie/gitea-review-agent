package console

import (
	"strings"
	"testing"

	"github.com/turning4th/codex-gitea/internal/model"
)

func TestBuildProjectSkillPromptAsksForGeneralRulesNotFindingList(t *testing.T) {
	prompt := BuildProjectSkillPrompt(model.SkillGenerationInput{
		Context: model.ProjectSkillContext{
			Owner: "acme",
			Repo:  "widgets",
			Slug:  "acme-widgets",
			Patterns: []model.ProjectSkillPattern{{
				Title:      "nil panic",
				Count:      3,
				SamplePath: "service.go",
				SampleLine: 42,
			}},
		},
		Existing: &model.ProjectSkill{Content: "old guidance"},
	})

	for _, want := range []string{
		"Write a reusable project guide, not a report",
		"do not enumerate the discovered findings one by one",
		"Convert repeated findings into general patterns",
		"Include only a few representative examples",
		"Evolve the existing skill instead of replacing it from scratch",
		`"acme-widgets"`,
	} {
		if !strings.Contains(prompt, want) {
			t.Fatalf("prompt missing %q:\n%s", want, prompt)
		}
	}
}
