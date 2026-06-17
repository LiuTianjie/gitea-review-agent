// Package review wires the modules into the end-to-end review pipeline.
package review

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"

	"github.com/turning4th/codex-gitea/internal/model"
)

// Orchestrator runs a single job through gitcache -> codex -> gitea.
type Orchestrator struct {
	Store     model.Store
	Cache     model.GitCache
	Reviewers []model.Reviewer
	// ReviewersFunc is used by production wiring so console-updated reviewer
	// settings take effect for subsequent jobs without restarting the service.
	ReviewersFunc func() []model.Reviewer
	Gitea         model.GiteaClient
	BaseRef       string // unused; per-PR base comes from the event

	TriggerKeywords []string
	RepoAllowlist   []string // entries "owner/repo"; empty = allow all
	// TriggerKeywordsFunc and RepoAllowlistFunc are used by production wiring
	// so console-updated settings apply to subsequent jobs without restart.
	TriggerKeywordsFunc func() []string
	RepoAllowlistFunc   func() []string
	Logger              *log.Logger
}

var errPullRequestClosed = errors.New("pull request is closed or merged")

type worktreeDiffSummary struct {
	BaseRef   string
	HeadSHA   string
	FileCount int
	Sample    []string
	Files     []string
	Numstat   []string
}

type guidanceFile struct {
	Path      string
	Contents  string
	Truncated bool
}

func (o *Orchestrator) logf(format string, args ...any) {
	if o.Logger != nil {
		o.Logger.Printf(format, args...)
	}
}

func (o *Orchestrator) reviewers() []model.Reviewer {
	if o.ReviewersFunc != nil {
		return o.ReviewersFunc()
	}
	if len(o.Reviewers) > 0 {
		return o.Reviewers
	}
	return nil
}

func (o *Orchestrator) primaryReviewer() model.Reviewer {
	for _, r := range o.reviewers() {
		if r.Name() == "codex" {
			return r
		}
	}
	if rs := o.reviewers(); len(rs) > 0 {
		return rs[0]
	}
	return nil
}

func (o *Orchestrator) jobLog(ctx context.Context, jobID int64, stage, message string) {
	if o.Store != nil {
		if err := o.Store.AppendJobLog(ctx, jobID, stage, message); err != nil {
			o.logf("job %d log %s: %v", jobID, stage, err)
		}
	}
	o.logf("job %d [%s] %s", jobID, stage, message)
}

// Process decodes a job and dispatches by event type/action.
func (o *Orchestrator) Process(ctx context.Context, job *model.Job) error {
	var ev model.WebhookEvent
	if err := json.Unmarshal(job.Payload, &ev); err != nil {
		return fmt.Errorf("decode payload: %w", err)
	}
	o.jobLog(ctx, job.ID, "event", fmt.Sprintf("%s %s %s/%s#%d", ev.Event, ev.Action, ev.PR.Owner, ev.PR.Repo, ev.PR.Number))

	if !o.allowed(ev.PR) {
		o.jobLog(ctx, job.ID, "skip", "repo not in allowlist")
		return nil
	}

	switch ev.Event {
	case model.EventPullRequest:
		return o.handlePR(ctx, job.ID, &ev)
	case model.EventIssueComment:
		return o.handleComment(ctx, job.ID, &ev)
	default:
		o.jobLog(ctx, job.ID, "skip", fmt.Sprintf("ignoring event type %q", ev.Event))
		return nil
	}
}

func (o *Orchestrator) allowed(pr model.PRRef) bool {
	allowlist := o.repoAllowlist()
	if len(allowlist) == 0 {
		return true
	}
	full := pr.Owner + "/" + pr.Repo
	for _, a := range allowlist {
		if strings.EqualFold(strings.TrimSpace(a), full) {
			return true
		}
	}
	return false
}

func (o *Orchestrator) handlePR(ctx context.Context, jobID int64, ev *model.WebhookEvent) error {
	switch ev.Action {
	case "opened", "reopened":
		return o.review(ctx, jobID, ev, false)
	case "synchronized":
		return o.review(ctx, jobID, ev, true)
	case "closed":
		o.jobLog(ctx, jobID, "cleanup", "PR closed; cleaning cached worktree")
		if err := o.Cache.Cleanup(ev.PR); err != nil {
			o.jobLog(ctx, jobID, "cleanup", "failed: "+err.Error())
			return err
		}
		o.jobLog(ctx, jobID, "cleanup", "done")
		return nil
	default:
		o.jobLog(ctx, jobID, "skip", fmt.Sprintf("ignoring PR action %q", ev.Action))
		return nil
	}
}

func inspectWorktreeDiff(ctx context.Context, worktree, baseRef string) (worktreeDiffSummary, error) {
	if strings.TrimSpace(baseRef) == "" {
		baseRef = "main"
	}
	head, err := runGitOutput(ctx, worktree, "rev-parse", "--short=12", "HEAD")
	if err != nil {
		return worktreeDiffSummary{}, fmt.Errorf("rev-parse head: %w", err)
	}
	filesRaw, err := runGitOutput(ctx, worktree, "diff", "--name-only", baseRef+"...HEAD")
	if err != nil {
		return worktreeDiffSummary{}, fmt.Errorf("diff --name-only %s...HEAD: %w", baseRef, err)
	}
	files := nonEmptyLines(filesRaw)
	numstatRaw, err := runGitOutput(ctx, worktree, "diff", "--numstat", baseRef+"...HEAD")
	if err != nil {
		return worktreeDiffSummary{}, fmt.Errorf("diff --numstat %s...HEAD: %w", baseRef, err)
	}
	summary := worktreeDiffSummary{
		BaseRef:   baseRef,
		HeadSHA:   strings.TrimSpace(head),
		FileCount: len(files),
		Files:     files,
		Numstat:   nonEmptyLines(numstatRaw),
	}
	if len(files) > 5 {
		summary.Sample = files[:5]
	} else {
		summary.Sample = files
	}
	return summary, nil
}

func runGitOutput(ctx context.Context, worktree string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.Dir = worktree
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		msg := strings.TrimSpace(stderr.String())
		if msg != "" {
			return "", fmt.Errorf("%w: %s", err, msg)
		}
		return "", err
	}
	return stdout.String(), nil
}

func nonEmptyLines(raw string) []string {
	lines := strings.Split(raw, "\n")
	out := make([]string, 0, len(lines))
	for _, line := range lines {
		line = strings.TrimSpace(line)
		if line != "" {
			out = append(out, line)
		}
	}
	return out
}

func formatWorktreeDiffSummary(summary worktreeDiffSummary) string {
	msg := fmt.Sprintf("worktree diff: base=%s head=%s files=%d", summary.BaseRef, summary.HeadSHA, summary.FileCount)
	if len(summary.Sample) > 0 {
		msg += " sample=" + strings.Join(summary.Sample, ", ")
		if summary.FileCount > len(summary.Sample) {
			msg += fmt.Sprintf(" (+%d more)", summary.FileCount-len(summary.Sample))
		}
	}
	return msg
}

func appendDiffInventoryNote(note string, summary worktreeDiffSummary) string {
	if summary.FileCount == 0 && len(summary.Files) == 0 && len(summary.Numstat) == 0 {
		return note
	}
	const maxInventoryLines = 200
	var b strings.Builder
	if strings.TrimSpace(note) != "" {
		b.WriteString(strings.TrimSpace(note))
		b.WriteString("\n\n")
	}
	b.WriteString("Current PR diff inventory (authoritative coverage checklist):\n")
	fmt.Fprintf(&b, "- base: %s\n", summary.BaseRef)
	fmt.Fprintf(&b, "- head: %s\n", summary.HeadSHA)
	fmt.Fprintf(&b, "- changed_files: %d\n", summary.FileCount)
	b.WriteString("- Use this list to make sure the first review covers every relevant changed file and its risky call paths. Do not stop after the first valid issue.\n")
	if len(summary.Numstat) > 0 {
		b.WriteString("- numstat entries are: added<TAB>deleted<TAB>path\n")
		lines := summary.Numstat
		if len(lines) > maxInventoryLines {
			lines = lines[:maxInventoryLines]
		}
		for _, line := range lines {
			b.WriteString("  - ")
			b.WriteString(line)
			b.WriteString("\n")
		}
		if len(summary.Numstat) > maxInventoryLines {
			fmt.Fprintf(&b, "  - ... %d more files omitted from this note; run git diff --name-only %s...HEAD to inspect the rest.\n", len(summary.Numstat)-maxInventoryLines, summary.BaseRef)
		}
	} else if len(summary.Files) > 0 {
		files := summary.Files
		if len(files) > maxInventoryLines {
			files = files[:maxInventoryLines]
		}
		for _, file := range files {
			b.WriteString("  - ")
			b.WriteString(file)
			b.WriteString("\n")
		}
		if len(summary.Files) > maxInventoryLines {
			fmt.Fprintf(&b, "  - ... %d more files omitted from this note; run git diff --name-only %s...HEAD to inspect the rest.\n", len(summary.Files)-maxInventoryLines, summary.BaseRef)
		}
	}
	return strings.TrimRight(b.String(), "\n")
}

func collectRepositoryGuidance(worktree string) ([]guidanceFile, error) {
	const (
		maxFiles        = 24
		maxBytesPerFile = 12 * 1024
	)
	worktree = strings.TrimSpace(worktree)
	if worktree == "" {
		return nil, nil
	}
	skipDirs := map[string]bool{
		".git": true, ".cache": true, ".next": true, ".turbo": true,
		"build": true, "coverage": true, "dist": true, "node_modules": true,
		"target": true, "vendor": true,
	}
	var paths []string
	err := filepath.WalkDir(worktree, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		rel, relErr := filepath.Rel(worktree, path)
		if relErr != nil || rel == "." {
			return nil
		}
		rel = filepath.ToSlash(rel)
		if d.IsDir() {
			if skipDirs[d.Name()] {
				return filepath.SkipDir
			}
			return nil
		}
		if isRepositoryGuidancePath(rel) {
			paths = append(paths, rel)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	sort.Slice(paths, func(i, j int) bool {
		pi, pj := guidancePriority(paths[i]), guidancePriority(paths[j])
		if pi != pj {
			return pi < pj
		}
		return paths[i] < paths[j]
	})
	if len(paths) > maxFiles {
		paths = paths[:maxFiles]
	}
	out := make([]guidanceFile, 0, len(paths))
	for _, rel := range paths {
		data, err := os.ReadFile(filepath.Join(worktree, filepath.FromSlash(rel)))
		if err != nil {
			continue
		}
		truncated := false
		if len(data) > maxBytesPerFile {
			data = data[:maxBytesPerFile]
			truncated = true
		}
		text := strings.TrimSpace(string(data))
		if text == "" {
			continue
		}
		out = append(out, guidanceFile{Path: rel, Contents: text, Truncated: truncated})
	}
	return out, nil
}

func isRepositoryGuidancePath(rel string) bool {
	base := strings.ToLower(filepath.Base(rel))
	switch base {
	case "agents.md", "claude.md", "readme.md", "code_review.md", "coding_standards.md", "contributing.md", "security.md", ".coderabbit.yaml":
		return true
	}
	lower := strings.ToLower(rel)
	if lower == ".github/copilot-instructions.md" {
		return true
	}
	if strings.HasPrefix(lower, ".github/instructions/") && strings.HasSuffix(lower, ".instructions.md") {
		return true
	}
	if strings.HasPrefix(lower, ".greptile/") {
		return true
	}
	return false
}

func guidancePriority(path string) int {
	lower := strings.ToLower(path)
	base := strings.ToLower(filepath.Base(lower))
	if filepath.Dir(lower) == "." {
		switch base {
		case "agents.md":
			return 0
		case "claude.md":
			return 1
		case "code_review.md", "coding_standards.md", "contributing.md":
			return 2
		case "readme.md":
			return 3
		case "security.md":
			return 4
		}
	}
	if strings.HasPrefix(lower, ".github/") {
		return 5
	}
	if strings.HasPrefix(lower, ".greptile/") || base == ".coderabbit.yaml" {
		return 6
	}
	switch base {
	case "agents.md":
		return 7
	case "claude.md":
		return 8
	case "readme.md":
		return 9
	default:
		return 10
	}
}

func appendRepositoryGuidanceNote(note string, files []guidanceFile) string {
	if len(files) == 0 {
		return note
	}
	var b strings.Builder
	if strings.TrimSpace(note) != "" {
		b.WriteString(strings.TrimSpace(note))
		b.WriteString("\n\n")
	}
	b.WriteString("Repository guidance context (read this before reviewing the diff):\n")
	b.WriteString("- These files were found automatically in the checked-out repository before review. Treat them as repository-specific instructions and apply path-specific guidance only where it matches.\n")
	b.WriteString("- Use README content for project intent, setup assumptions, and documented contracts; use AGENTS/CLAUDE/CODE_REVIEW style files as direct review guidance.\n")
	for _, f := range files {
		fmt.Fprintf(&b, "\n--- %s", f.Path)
		if f.Truncated {
			b.WriteString(" (truncated)")
		}
		b.WriteString(" ---\n")
		b.WriteString(f.Contents)
		b.WriteString("\n")
	}
	return strings.TrimRight(b.String(), "\n")
}

func (o *Orchestrator) ensurePullRequestOpen(ctx context.Context, jobID int64, pr model.PRRef) error {
	status, err := o.Gitea.GetPullRequestStatus(ctx, pr)
	if err != nil {
		return fmt.Errorf("check PR status: %w", err)
	}
	if status.Open() {
		return nil
	}
	o.jobLog(ctx, jobID, "skip", fmt.Sprintf("PR is no longer open (state=%s merged=%t); skipping review submission", status.State, status.Merged))
	return errPullRequestClosed
}

// review runs a full (new) or incremental (resume) review and posts it.
func (o *Orchestrator) review(ctx context.Context, jobID int64, ev *model.WebhookEvent, isUpdate bool) error {
	pr := ev.PR

	if len(o.reviewers()) == 0 {
		return fmt.Errorf("no reviewers configured")
	}
	if err := o.ensurePullRequestOpen(ctx, jobID, pr); err != nil {
		if errors.Is(err, errPullRequestClosed) {
			return nil
		}
		return err
	}

	o.jobLog(ctx, jobID, "state", "loading PR state")
	pull, err := o.Store.GetPull(ctx, pr)
	if err != nil {
		return fmt.Errorf("get pull: %w", err)
	}
	if pull == nil {
		if err := o.Store.UpsertPull(ctx, &model.PullState{PR: pr, Author: ev.Author, HeadSHA: ev.HeadSHA, BaseRef: ev.BaseRef}); err != nil {
			return fmt.Errorf("ensure pull: %w", err)
		}
		pull, err = o.Store.GetPull(ctx, pr)
		if err != nil {
			return fmt.Errorf("get ensured pull: %w", err)
		}
	}
	if pull == nil {
		return fmt.Errorf("pull state was not created")
	}

	o.jobLog(ctx, jobID, "git", "preparing worktree")
	worktree, err := o.Cache.Prepare(ctx, pr, ev.CloneURL, ev.BaseRef, ev.HeadRef, ev.HeadSHA)
	if err != nil {
		return fmt.Errorf("git prepare: %w", err)
	}
	defer func() {
		if cerr := o.Cache.Cleanup(pr); cerr != nil {
			o.logf("cleanup %s#%d: %v", pr.Repo, pr.Number, cerr)
		}
	}()
	o.jobLog(ctx, jobID, "git", "worktree ready")
	var diffSummary worktreeDiffSummary
	if summary, err := inspectWorktreeDiff(ctx, worktree, ev.BaseRef); err != nil {
		o.jobLog(ctx, jobID, "git", "worktree diff summary failed: "+err.Error())
	} else {
		diffSummary = summary
		o.jobLog(ctx, jobID, "git", formatWorktreeDiffSummary(summary))
	}
	guidance, err := collectRepositoryGuidance(worktree)
	if err != nil {
		o.jobLog(ctx, jobID, "git", "repository guidance scan failed: "+err.Error())
	} else if len(guidance) > 0 {
		paths := make([]string, 0, len(guidance))
		for _, f := range guidance {
			paths = append(paths, f.Path)
		}
		o.jobLog(ctx, jobID, "git", "repository guidance found: "+strings.Join(paths, ", "))
	} else {
		o.jobLog(ctx, jobID, "git", "repository guidance found: none")
	}

	o.jobLog(ctx, jobID, "gitea", "fetching PR diff")
	diff, err := o.Gitea.GetDiff(ctx, pr)
	if err != nil {
		return fmt.Errorf("get diff: %w", err)
	}
	o.jobLog(ctx, jobID, "gitea", fmt.Sprintf("diff fetched: files=%d", len(diff.Files)))

	o.jobLog(ctx, jobID, "gitea", "fetching PR comments")
	discussion, err := o.Gitea.ListIssueComments(ctx, pr)
	if err != nil {
		o.jobLog(ctx, jobID, "gitea", "PR comments unavailable: "+err.Error())
		discussion = nil
	} else {
		o.jobLog(ctx, jobID, "gitea", fmt.Sprintf("PR comments fetched: %d", len(discussion)))
	}

	var errs []error
	successes := 0
	for _, reviewer := range o.reviewers() {
		if err := o.runReviewer(ctx, jobID, ev, isUpdate, pull.ID, worktree, diff, diffSummary, guidance, discussion, reviewer); err != nil {
			if errors.Is(err, errPullRequestClosed) {
				o.jobLog(ctx, jobID, "skip", "PR closed or merged; stopping remaining reviewers")
				break
			}
			errs = append(errs, err)
			o.jobLog(ctx, jobID, reviewer.Name(), "failed: "+err.Error())
			continue
		}
		successes++
	}
	if successes == 0 && len(errs) > 0 {
		return errors.Join(errs...)
	}
	return nil
}

func (o *Orchestrator) runReviewer(ctx context.Context, jobID int64, ev *model.WebhookEvent, isUpdate bool, pullID int64, worktree string, diff model.DiffMap, diffSummary worktreeDiffSummary, guidance []guidanceFile, discussion []model.PullComment, reviewer model.Reviewer) error {
	pr := ev.PR
	agent := reviewer.Name()
	state, err := o.Store.GetReviewerState(ctx, pr, agent)
	if err != nil {
		return fmt.Errorf("%s get state: %w", agent, err)
	}
	sessionID := ""
	prevHead := ""
	var prevReviewID int64
	if state != nil {
		sessionID = state.SessionID
		prevHead = state.HeadSHA
		prevReviewID = state.LastReviewID
	}
	priorOpen, err := o.openFindings(ctx, pullID, agent)
	if err != nil {
		return err
	}

	run := &model.ReviewRun{PullID: pullID, JobID: jobID, Agent: agent, HeadSHA: ev.HeadSHA}
	if err := o.Store.CreateReviewRun(ctx, run); err != nil {
		o.logf("create review run: %v", err)
	}

	in := model.CodexInput{Worktree: worktree, BaseRef: ev.BaseRef}
	if isUpdate && sessionID != "" {
		in.SessionID = sessionID
		in.Note = fmt.Sprintf("PR head 已从 %s 更新到 %s。请基于 %s 重新审查当前 diff，并在摘要中说明之前报告的问题哪些已经修复。", shortSHA(prevHead), shortSHA(ev.HeadSHA), ev.BaseRef)
	}
	in.Note = appendRepositoryGuidanceNote(in.Note, guidance)
	in.Note = appendDiffInventoryNote(in.Note, diffSummary)
	in.Note = appendCommentContextNote(in.Note, discussion)
	in.Note = appendPriorFindingsNote(in.Note, priorOpen)

	o.jobLog(ctx, jobID, agent, "review started")
	result, err := reviewer.Review(ctx, in)
	if err != nil {
		if run.ID != 0 {
			_ = o.Store.FinishReviewRun(ctx, run.ID, model.ReviewRunFailed, err.Error(), 0)
		}
		return fmt.Errorf("%s review: %w", agent, err)
	}
	result.Findings = carryForwardFindings(result.Findings, priorOpen, result.ResolvedFingerprints, agent)
	result.OverallSeverity = highestSeverity(result.Findings, result.OverallSeverity)
	o.jobLog(ctx, jobID, agent, fmt.Sprintf("review finished: %d findings, session=%s", len(result.Findings), result.SessionID))

	comments, unmapped, mapped := mapFindings(diff, result.Findings, agent)
	if isUpdate && prevReviewID != 0 {
		o.jobLog(ctx, jobID, "gitea", fmt.Sprintf("dismissing previous %s review", agent))
		if err := o.Gitea.DismissReview(ctx, pr, prevReviewID, "新提交触发了重新审查，此前审查已被替代。"); err != nil {
			o.logf("dismiss prior %s review %d: %v", agent, prevReviewID, err)
		}
	}

	body := buildSummary(agent, result, unmapped)
	event := model.ReviewEventComment
	if result.OverallSeverity.Blocking() || anyBlocking(result.Findings) {
		event = model.ReviewEventRequestChanges
	}
	if err := o.ensurePullRequestOpen(ctx, jobID, pr); err != nil {
		if errors.Is(err, errPullRequestClosed) {
			if run.ID != 0 {
				_ = o.Store.FinishReviewRun(ctx, run.ID, model.ReviewRunSkipped, "PR closed or merged before posting review", len(result.Findings))
			}
		}
		return err
	}
	o.jobLog(ctx, jobID, "gitea", fmt.Sprintf("posting %s review: %d inline comments, verdict=%s", agent, len(comments), event))
	reviewID, err := o.Gitea.PostReview(ctx, pr, ev.HeadSHA, event, body, comments)
	if err != nil {
		if isClosedReviewError(err) {
			o.jobLog(ctx, jobID, "skip", "PR closed or merged while posting review; skipping remaining reviewers")
			if run.ID != 0 {
				_ = o.Store.FinishReviewRun(ctx, run.ID, model.ReviewRunSkipped, err.Error(), len(result.Findings))
			}
			return errPullRequestClosed
		}
		if run.ID != 0 {
			_ = o.Store.FinishReviewRun(ctx, run.ID, model.ReviewRunFailed, err.Error(), len(result.Findings))
		}
		return fmt.Errorf("%s post review: %w", agent, err)
	}
	o.jobLog(ctx, jobID, "gitea", fmt.Sprintf("%s review posted: id=%d", agent, reviewID))

	st := &model.PullReviewerState{
		PR: pr, Agent: agent, SessionID: result.SessionID, HeadSHA: ev.HeadSHA,
		BaseRef: ev.BaseRef, LastReviewID: reviewID,
	}
	if st.SessionID == "" {
		st.SessionID = sessionID
	}
	if err := o.Store.UpsertReviewerState(ctx, st); err != nil {
		o.logf("upsert %s state: %v", agent, err)
	}
	if err := o.Store.SaveFindings(ctx, pullID, ev.HeadSHA, result.Findings, model.FindingSaveOptions{
		Agent: agent, ReviewRunID: run.ID, MappedFingerprints: mapped,
	}); err != nil {
		o.logf("save %s findings: %v", agent, err)
	}
	if err := o.Store.MarkFindingsFixed(ctx, pullID, resolvedOnly(result.ResolvedFingerprints, result.Findings, agent)); err != nil {
		o.logf("mark %s fixed findings: %v", agent, err)
	}
	if run.ID != 0 {
		_ = o.Store.FinishReviewRun(ctx, run.ID, model.ReviewRunDone, "", len(result.Findings))
	}
	o.logf("reviewed %s/%s#%d with %s: %d findings (%d inline), verdict=%s, session=%s",
		pr.Owner, pr.Repo, pr.Number, agent, len(result.Findings), len(comments), event, result.SessionID)
	return nil
}

func isClosedReviewError(err error) bool {
	if err == nil {
		return false
	}
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "closed or merged pr") ||
		strings.Contains(msg, "closed or merged pull request") ||
		strings.Contains(msg, "can't submit review for a closed")
}

func appendCommentContextNote(note string, comments []model.PullComment) string {
	context := formatCommentContext(comments)
	if context == "" {
		return note
	}
	if strings.TrimSpace(note) == "" {
		return context
	}
	return strings.TrimSpace(note) + "\n\n" + context
}

func formatCommentContext(comments []model.PullComment) string {
	if len(comments) == 0 {
		return ""
	}
	const (
		maxComments = 8
		maxBody     = 800
	)
	start := 0
	if len(comments) > maxComments {
		start = len(comments) - maxComments
	}
	var b strings.Builder
	b.WriteString("PR discussion context:\n")
	b.WriteString("- The following recent PR issue-thread comments may contain human review requests, constraints, or clarifications.\n")
	b.WriteString("- Use them to prioritize what to inspect, but only report findings that are supported by the current PR diff or related code evidence.\n")
	wrote := false
	for _, c := range comments[start:] {
		body := strings.TrimSpace(c.Body)
		if body == "" {
			continue
		}
		user := strings.TrimSpace(c.User)
		if user == "" {
			user = "unknown"
		}
		body = strings.Join(strings.Fields(body), " ")
		if len(body) > maxBody {
			body = body[:maxBody] + "..."
		}
		if c.CreatedAt.IsZero() {
			fmt.Fprintf(&b, "- %s: %s\n", user, body)
			wrote = true
			continue
		}
		fmt.Fprintf(&b, "- %s at %s: %s\n", user, c.CreatedAt.Format("2006-01-02 15:04:05"), body)
		wrote = true
	}
	if !wrote {
		return ""
	}
	return strings.TrimRight(b.String(), "\n")
}

// handleComment answers a bot-directed question on a PR by resuming its session.
func (o *Orchestrator) handleComment(ctx context.Context, jobID int64, ev *model.WebhookEvent) error {
	if !ev.IsPR || ev.Action != "created" {
		o.jobLog(ctx, jobID, "skip", "comment is not a new PR comment")
		return nil
	}
	question, ok := o.matchTrigger(ev.CommentBody)
	if !ok {
		o.jobLog(ctx, jobID, "skip", "comment does not match trigger keywords")
		return nil
	}

	o.jobLog(ctx, jobID, "state", "loading prior PR session")
	reviewers, question := o.commentReviewers(question)
	if len(reviewers) == 0 {
		return fmt.Errorf("no reviewer configured")
	}

	answers := make([]string, 0, len(reviewers))
	for _, reviewer := range reviewers {
		answer, err := o.askReviewer(ctx, jobID, ev, reviewer, question)
		if err != nil {
			return err
		}
		if len(reviewers) == 1 {
			answers = append(answers, answer)
		} else {
			answers = append(answers, "## "+reviewerDisplayName(reviewer.Name())+"\n\n"+answer)
		}
	}
	o.jobLog(ctx, jobID, "gitea", "posting answer comment")
	_, err := o.Gitea.PostComment(ctx, ev.PR, strings.Join(answers, "\n\n---\n\n"))
	return err
}

func (o *Orchestrator) askReviewer(ctx context.Context, jobID int64, ev *model.WebhookEvent, reviewer model.Reviewer, question string) (string, error) {
	prev, err := o.Store.GetReviewerState(ctx, ev.PR, reviewer.Name())
	if err != nil {
		return "", fmt.Errorf("%s get pull: %w", reviewer.Name(), err)
	}
	if prev == nil || prev.SessionID == "" {
		o.jobLog(ctx, jobID, "gitea", "missing session for "+reviewer.Name())
		return "我还没有使用 " + reviewerDisplayName(reviewer.Name()) + " 审查过这个 PR，因此没有可继续的会话。请先推送一次提交或重新打开 PR 来触发审查。", nil
	}

	o.jobLog(ctx, jobID, "git", "preparing worktree for "+reviewer.Name())
	worktree, err := o.Cache.Prepare(ctx, ev.PR, ev.CloneURL, prev.BaseRef, ev.HeadRef, prev.HeadSHA)
	if err != nil {
		return "", fmt.Errorf("git prepare: %w", err)
	}
	defer o.Cache.Cleanup(ev.PR)
	o.jobLog(ctx, jobID, "git", "worktree ready for "+reviewer.Name())

	o.jobLog(ctx, jobID, reviewer.Name(), "answer started")
	answer, err := reviewer.Ask(ctx, prev.SessionID, worktree, question)
	if err != nil {
		return "", fmt.Errorf("%s ask: %w", reviewer.Name(), err)
	}
	o.jobLog(ctx, jobID, reviewer.Name(), "answer finished")
	return answer, nil
}

func (o *Orchestrator) commentReviewers(question string) ([]model.Reviewer, string) {
	reviewers := o.reviewers()
	if len(reviewers) == 0 {
		return nil, question
	}
	trimmed := strings.TrimSpace(question)
	first, rest, hasFirst := strings.Cut(trimmed, " ")
	if !hasFirst {
		first = trimmed
		rest = ""
	}
	target := strings.ToLower(strings.TrimSpace(first))
	if target == "all" {
		if strings.TrimSpace(rest) == "" {
			rest = "请重新审查当前 diff，并回答仍然需要关注的问题。"
		}
		return reviewers, strings.TrimSpace(rest)
	}
	for _, reviewer := range reviewers {
		if normalizeAgentName(reviewer.Name()) == target {
			if strings.TrimSpace(rest) == "" {
				rest = "请重新审查当前 diff，并回答仍然需要关注的问题。"
			}
			return []model.Reviewer{reviewer}, strings.TrimSpace(rest)
		}
	}
	if primary := o.primaryReviewer(); primary != nil {
		return []model.Reviewer{primary}, question
	}
	return nil, question
}

// matchTrigger returns the text following a trigger keyword, if present.
func (o *Orchestrator) matchTrigger(body string) (string, bool) {
	for _, kw := range o.triggerKeywords() {
		kw = strings.TrimSpace(kw)
		if kw == "" {
			continue
		}
		if idx := strings.Index(body, kw); idx >= 0 {
			rest := strings.TrimSpace(body[idx+len(kw):])
			if rest == "" {
				rest = "请重新审查当前 diff，并回答仍然需要关注的问题。"
			}
			return rest, true
		}
	}
	return "", false
}

func (o *Orchestrator) triggerKeywords() []string {
	if o.TriggerKeywordsFunc != nil {
		return o.TriggerKeywordsFunc()
	}
	return o.TriggerKeywords
}

func (o *Orchestrator) repoAllowlist() []string {
	if o.RepoAllowlistFunc != nil {
		return o.RepoAllowlistFunc()
	}
	return o.RepoAllowlist
}

// ---------- helpers ----------

func (o *Orchestrator) openFindings(ctx context.Context, pullID int64, agent string) ([]model.StoredFinding, error) {
	stored, err := o.Store.ListFindings(ctx, pullID)
	if err != nil {
		return nil, fmt.Errorf("list prior findings: %w", err)
	}
	open := make([]model.StoredFinding, 0, len(stored))
	for _, sf := range stored {
		if (sf.Status == "" || sf.Status == "open") && normalizeAgentName(sf.Agent) == normalizeAgentName(agent) {
			open = append(open, sf)
		}
	}
	return open, nil
}

func appendPriorFindingsNote(note string, prior []model.StoredFinding) string {
	if len(prior) == 0 {
		return note
	}
	type priorFindingNote struct {
		Fingerprint  string         `json:"fingerprint"`
		Path         string         `json:"path"`
		Line         int            `json:"line"`
		Side         model.Side     `json:"side"`
		Severity     model.Severity `json:"severity"`
		Title        string         `json:"title"`
		Body         string         `json:"body"`
		FirstSeenSHA string         `json:"first_seen_sha,omitempty"`
	}
	items := make([]priorFindingNote, 0, len(prior))
	for _, sf := range prior {
		items = append(items, priorFindingNote{
			Fingerprint:  sf.Fingerprint,
			Path:         sf.Path,
			Line:         sf.Line,
			Side:         sf.Side,
			Severity:     sf.Severity,
			Title:        sf.Title,
			Body:         sf.Body,
			FirstSeenSHA: sf.FirstSeenSHA,
		})
	}
	b, err := json.Marshal(items)
	if err != nil {
		return note
	}
	var out strings.Builder
	if strings.TrimSpace(note) != "" {
		out.WriteString(strings.TrimSpace(note))
		out.WriteString("\n\n")
	}
	out.WriteString("以下是此前仍处于 open 状态的问题，请逐条复核：\n")
	out.Write(b)
	out.WriteString("\n\n如果某个历史问题仍然存在，或你无法从当前代码明确确认已经修复，请把它继续放入 findings。只有在确认已经修复时，才从 findings 中省略，并把对应 fingerprint 放入 resolved_fingerprints。")
	return out.String()
}

func carryForwardFindings(current []model.Finding, prior []model.StoredFinding, resolved []string, agent string) []model.Finding {
	if len(prior) == 0 {
		return current
	}
	resolvedSet := fingerprintSet(resolved, agent)
	seen := make(map[string]bool, len(current))
	out := make([]model.Finding, 0, len(current)+len(prior))
	for _, f := range current {
		fp := effectiveFindingFingerprint(agent, f.Fingerprint())
		seen[fp] = true
		out = append(out, f)
	}
	for _, sf := range prior {
		fp := strings.TrimSpace(sf.Fingerprint)
		if fp == "" {
			fp = effectiveFindingFingerprint(agent, sf.Finding.Fingerprint())
		}
		if resolvedSet[fp] || seen[fp] {
			continue
		}
		f := sf.Finding
		if strings.TrimSpace(f.Title) == "" {
			f.Title = "Previously reported issue"
		}
		if strings.TrimSpace(f.Body) == "" {
			f.Body = "This finding was reported in an earlier review and was not explicitly marked fixed in this run."
		}
		out = append(out, f)
		seen[fp] = true
	}
	return out
}

func highestSeverity(fs []model.Finding, _ model.Severity) model.Severity {
	if len(fs) == 0 {
		return model.Severity("none")
	}
	best := model.SeverityInfo
	for _, f := range fs {
		if severityRank(f.Severity) > severityRank(best) {
			best = f.Severity
		}
	}
	if best == model.SeverityInfo {
		return model.SeverityLow
	}
	return best
}

func severityRank(s model.Severity) int {
	switch s {
	case model.SeverityInfo:
		return 0
	case model.SeverityLow:
		return 1
	case model.SeverityMedium:
		return 2
	case model.SeverityHigh:
		return 3
	case model.SeverityCritical:
		return 4
	default:
		return -1
	}
}

func resolvedOnly(resolved []string, findings []model.Finding, agent string) []string {
	if len(resolved) == 0 {
		return nil
	}
	current := make(map[string]bool, len(findings))
	for _, f := range findings {
		current[effectiveFindingFingerprint(agent, f.Fingerprint())] = true
	}
	out := make([]string, 0, len(resolved))
	seen := map[string]bool{}
	for _, fp := range resolved {
		fp = strings.TrimSpace(fp)
		if fp == "" {
			continue
		}
		candidates := fingerprintCandidates(fp, agent)
		for _, candidate := range candidates {
			if current[candidate] || seen[candidate] {
				continue
			}
			out = append(out, candidate)
			seen[candidate] = true
		}
	}
	return out
}

func normalizeAgentName(agent string) string {
	agent = strings.ToLower(strings.TrimSpace(agent))
	if agent == "" {
		return "codex"
	}
	return agent
}

func effectiveFindingFingerprint(agent, fp string) string {
	agent = normalizeAgentName(agent)
	fp = strings.TrimSpace(fp)
	if strings.HasPrefix(fp, agent+":") {
		return fp
	}
	return agent + ":" + fp
}

func stringSet(items []string) map[string]bool {
	out := make(map[string]bool, len(items))
	for _, item := range items {
		item = strings.TrimSpace(item)
		if item != "" {
			out[item] = true
		}
	}
	return out
}

func fingerprintSet(items []string, agent string) map[string]bool {
	out := map[string]bool{}
	for _, item := range items {
		for _, candidate := range fingerprintCandidates(item, agent) {
			out[candidate] = true
		}
	}
	return out
}

func fingerprintCandidates(fp, agent string) []string {
	fp = strings.TrimSpace(fp)
	if fp == "" {
		return nil
	}
	out := []string{fp}
	if !strings.Contains(fp, ":") {
		out = append(out, effectiveFindingFingerprint(agent, fp))
	}
	return out
}

func mapFindings(diff model.DiffMap, fs []model.Finding, agent string) (comments []model.ReviewComment, unmapped []model.Finding, mapped map[string]bool) {
	mapped = map[string]bool{}
	for _, f := range fs {
		newPos, oldPos, ok := diff.Position(f.Path, f.Line, f.Side)
		if !ok {
			unmapped = append(unmapped, f)
			continue
		}
		mapped[effectiveFindingFingerprint(agent, f.Fingerprint())] = true
		comments = append(comments, model.ReviewComment{
			Path:        f.Path,
			Body:        formatFinding(f),
			NewPosition: newPos,
			OldPosition: oldPos,
		})
	}
	return comments, unmapped, mapped
}

func formatFinding(f model.Finding) string {
	return fmt.Sprintf("**[%s] %s**\n\n%s", severityLabel(f.Severity), f.Title, f.Body)
}

func buildSummary(agent string, r *model.ReviewResult, unmapped []model.Finding) string {
	var b strings.Builder
	b.WriteString("## ")
	b.WriteString(reviewerDisplayName(agent))
	b.WriteString(" 审查\n\n")
	writeFindingsOverview(&b, r.Findings)
	if strings.TrimSpace(r.Summary) != "" {
		b.WriteString("\n### 总结\n\n")
		b.WriteString(strings.TrimSpace(r.Summary))
	}
	b.WriteString(fmt.Sprintf("\n\n整体严重程度：**%s**\n", severityLabel(r.OverallSeverity)))
	if len(unmapped) > 0 {
		b.WriteString("\n### 无法映射到 diff 行的问题\n\n")
		for _, f := range unmapped {
			b.WriteString(fmt.Sprintf("- **[%s] %s** (`%s:%d`): %s\n",
				severityLabel(f.Severity), f.Title, f.Path, f.Line, f.Body))
		}
	}
	b.WriteString("\n_仅静态审查，未构建或执行代码。_")
	return b.String()
}

func writeFindingsOverview(b *strings.Builder, findings []model.Finding) {
	b.WriteString("### 问题总览\n\n")
	if len(findings) == 0 {
		b.WriteString("未发现需要处理的问题。\n")
		return
	}
	for _, f := range findings {
		b.WriteString(fmt.Sprintf("- **[%s] %s** (`%s:%d`): %s\n",
			severityLabel(f.Severity), strings.TrimSpace(f.Title), f.Path, f.Line, oneLine(f.Body)))
	}
}

func oneLine(s string) string {
	return strings.Join(strings.Fields(strings.TrimSpace(s)), " ")
}

func severityLabel(severity model.Severity) string {
	switch severity {
	case model.SeverityCritical:
		return "严重"
	case model.SeverityHigh:
		return "高"
	case model.SeverityMedium:
		return "中"
	case model.SeverityLow:
		return "低"
	case model.SeverityInfo:
		return "提示"
	case "":
		return "无"
	default:
		if strings.EqualFold(string(severity), "none") {
			return "无"
		}
		return string(severity)
	}
}

func reviewerDisplayName(agent string) string {
	switch normalizeAgentName(agent) {
	case "codex":
		return "Codex"
	case "claude":
		return "Claude"
	case "minimax":
		return "MiniMax"
	default:
		name := normalizeAgentName(agent)
		if name == "" {
			return "Reviewer"
		}
		return strings.ToUpper(name[:1]) + name[1:]
	}
}

func anyBlocking(fs []model.Finding) bool {
	for _, f := range fs {
		if f.Severity.Blocking() {
			return true
		}
	}
	return false
}

func shortSHA(s string) string {
	if len(s) > 8 {
		return s[:8]
	}
	if s == "" {
		return "(none)"
	}
	return s
}
