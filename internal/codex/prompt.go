package codex

import (
	"fmt"
	"strings"
)

const reviewLanguageInstruction = `Language rules:
- Write all human-facing review output in Simplified Chinese by default, including summary, finding titles, finding bodies, and answers to /review questions.
- Keep code, identifiers, file paths, command names, API names, error messages, and quoted source text in their original language.
- Do not use English prose unless the user's question explicitly asks for English.`

const reviewQualityInstruction = `Review process:
- First list changed files with ` + "`git diff --name-only <base>...HEAD`" + ` and make sure every relevant changed file is considered.
- Look for repository review guidance before judging findings. Read any present files that are relevant, such as ` + "`AGENTS.md`, `CLAUDE.md`, `CODE_REVIEW.md`, `CODING_STANDARDS.md`, `.github/copilot-instructions.md`, `.github/instructions/**/*.instructions.md`, `.coderabbit.yaml`, `.greptile/*`, `.cursorrules`" + `. Apply path-specific instructions only to matching files.
- Treat generated output, lock files, vendored dependencies, and build artifacts as low-value review targets unless the PR changes behavior through them or repository instructions explicitly include them.
- Prioritize correctness regressions, security/auth/input validation, API and data-contract breaks, persistence/migration compatibility, concurrency or async lifecycle bugs, resource leaks, error handling, and edge cases.
- Do not report pure style, readability, naming, formatting, or speculative maintainability feedback unless it creates a concrete bug risk or violates explicit repository instructions.
- Before returning JSON, verify every candidate finding has a concrete failure mode, is introduced or exposed by this PR, is not a duplicate, points to the best available line, and gives enough detail for a developer to fix it. Omit anything that fails this check.`

// buildReviewPrompt produces the prompt for a fresh structured review.
// It instructs codex to diff base..HEAD itself and report findings statically,
// without building/running/testing the code.
func buildReviewPrompt(baseRef, note string) string {
	if baseRef == "" {
		baseRef = "main"
	}
	var b strings.Builder
	fmt.Fprintf(&b, `You are a senior code reviewer performing a STATIC review of a pull request.

Run `+"`git diff %s...HEAD`"+` yourself to obtain the full pull-request diff from the target branch to the current HEAD.
Use that diff as the entrypoint, and inspect related functions, types, imports, call sites, and nearby files when needed to understand impact.

`+reviewQualityInstruction+`

Hard rules:
- Do NOT build, run, compile, or test the code. This is a static review only.
- Do NOT modify any files.
- You may run read-only inspection commands (git diff, nl, cat, grep) to understand the code.
- Base findings on risks introduced or exposed by this PR. You may cite surrounding or related code when it is necessary to explain a diff-rooted issue.
- Prefer findings on changed lines. If the best line for a real PR-caused issue is outside the diff, still report it; it will be included in the summary instead of as an inline comment.

`+reviewLanguageInstruction+`

For every concrete problem you find, emit a structured finding with:
- path: file path relative to the repository root (as shown by git diff).
- line: the line number the issue refers to.
- side: "NEW" for a line in the new/added version, "OLD" for a line in the old/removed version.
- severity: one of info, low, medium, high, critical.
- title: a short one-line summary of the issue.
- body: a clear explanation of the problem and why it matters.
- tags: zero to six short free-form issue tags, such as "auth", "migration", "concurrency", "data-loss", or "error-handling".

Also produce:
- summary: an overall summary of the review.
- overall_severity: the highest-impact severity across all findings (none, low, medium, high, critical); use "none" when there are no findings.
- resolved_fingerprints: include fingerprints for prior findings that are clearly fixed in the current diff; use an empty array when none are resolved or no prior findings are provided.

If there are no issues, return an empty findings array with overall_severity "none".
Return ONLY the structured JSON result conforming to the provided output schema.`, baseRef)

	if strings.TrimSpace(note) != "" {
		fmt.Fprintf(&b, "\n\nAdditional context:\n%s", note)
	}
	return b.String()
}

// buildResumePrompt produces the prompt for a resume (re-review) of an existing
// thread after the PR head moved. It folds in the optional note and asks codex
// to re-review and mark which previously reported issues are now fixed.
func buildResumePrompt(baseRef, note string) string {
	if baseRef == "" {
		baseRef = "main"
	}
	var b strings.Builder
	fmt.Fprintf(&b, `The pull request has been updated since your previous review. Re-review it STATICALLY.

Run `+"`git diff %s...HEAD`"+` again to obtain the full current pull-request diff from the target branch to the current HEAD.
Use that diff as the entrypoint, and inspect related functions, types, imports, call sites, and nearby files when needed to understand impact.

`+reviewQualityInstruction+`

Hard rules (unchanged):
- Do NOT build, run, compile, or test the code.
- Do NOT modify any files.
- Only read-only inspection commands are allowed.

`+reviewLanguageInstruction+`

Compare against your previous review:
- If prior open findings are provided below, re-check each one against the current diff and surrounding code.
- For prior findings that are still valid or uncertain, include them in findings again.
- For prior findings that are clearly fixed, do NOT include them in findings; instead include their fingerprint in resolved_fingerprints and mention in the summary that they were resolved.
- Report any remaining or newly introduced issues as fresh structured findings (path, line, side, severity, title, body).
- Include zero to six short free-form tags per finding to support aggregate reporting.
- Update overall_severity to reflect the current state (use "none" when no issues remain).

Return ONLY the structured JSON result conforming to the provided output schema.`, baseRef)

	if strings.TrimSpace(note) != "" {
		fmt.Fprintf(&b, "\n\nWhat changed since last review:\n%s", note)
	}
	return b.String()
}

func buildAskPrompt(question string) string {
	var b strings.Builder
	b.WriteString(reviewLanguageInstruction)
	b.WriteString("\n- Answer /review questions in plain Markdown text, not JSON.")
	b.WriteString("\n- If you re-evaluate findings, summarize them for a human reader instead of returning the structured review schema.")
	b.WriteString("\n\nUser question:\n")
	b.WriteString(strings.TrimSpace(question))
	return b.String()
}
