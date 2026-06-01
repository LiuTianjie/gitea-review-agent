package codex

import (
	"fmt"
	"strings"
)

const reviewLanguageInstruction = `Language rules:
- Write all human-facing review output in Simplified Chinese by default, including summary, finding titles, finding bodies, and answers to /review questions.
- Keep code, identifiers, file paths, command names, API names, error messages, and quoted source text in their original language.
- Do not use English prose unless the user's question explicitly asks for English.`

// buildReviewPrompt produces the prompt for a fresh structured review.
// It instructs codex to diff base..HEAD itself and report findings statically,
// without building/running/testing the code.
func buildReviewPrompt(baseRef, note string) string {
	if baseRef == "" {
		baseRef = "main"
	}
	var b strings.Builder
	fmt.Fprintf(&b, `You are a senior code reviewer performing a STATIC review of a pull request.

Run `+"`git diff %s...HEAD`"+` yourself to obtain the changes under review. Review ONLY the changed lines and the context needed to judge them.

Hard rules:
- Do NOT build, run, compile, or test the code. This is a static review only.
- Do NOT modify any files.
- You may run read-only inspection commands (git diff, nl, cat, grep) to understand the code.
- Base your review strictly on the diff content; do not invent issues outside the changed code.

`+reviewLanguageInstruction+`

For every concrete problem you find, emit a structured finding with:
- path: file path relative to the repository root (as shown by git diff).
- line: the line number the issue refers to.
- side: "NEW" for a line in the new/added version, "OLD" for a line in the old/removed version.
- severity: one of info, low, medium, high, critical.
- title: a short one-line summary of the issue.
- body: a clear explanation of the problem and why it matters.

Also produce:
- summary: an overall summary of the review.
- overall_severity: the highest-impact severity across all findings (none, low, medium, high, critical); use "none" when there are no findings.

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

Run `+"`git diff %s...HEAD`"+` again to obtain the current changes.

Hard rules (unchanged):
- Do NOT build, run, compile, or test the code.
- Do NOT modify any files.
- Only read-only inspection commands are allowed.

`+reviewLanguageInstruction+`

Compare against your previous review:
- For issues you reported earlier that are now fixed, do NOT include them in findings; mention in the summary that they were resolved.
- Report any remaining or newly introduced issues as fresh structured findings (path, line, side, severity, title, body).
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
