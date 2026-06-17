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
				Severity:   model.SeverityHigh,
				Status:     "open",
				Count:      3,
				OpenCount:  2,
				SamplePath: "service.go",
				SampleLine: 42,
				Tags:       []string{"runtime", "panic"},
			}},
		},
		Existing: &model.ProjectSkill{Content: "old guidance"},
	})

	for _, want := range []string{
		"Write a reusable project experience guide",
		"Do not reconstruct, quote, or invent concrete finding titles",
		"Convert evidence signals into general engineering lessons",
		"If the existing skill is too concrete",
		"Evolve the existing skill instead of replacing it from scratch",
		`"acme-widgets"`,
		`"runtime"`,
	} {
		if !strings.Contains(prompt, want) {
			t.Fatalf("prompt missing %q:\n%s", want, prompt)
		}
	}
	for _, notWant := range []string{
		"nil panic",
		"service.go",
		"sample_path",
		"sample_line",
	} {
		if strings.Contains(prompt, notWant) {
			t.Fatalf("prompt should not include concrete evidence %q:\n%s", notWant, prompt)
		}
	}
}
