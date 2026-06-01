// Package review wires the modules into the end-to-end review pipeline.
package review

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"strings"

	"github.com/turning4th/codex-gitea/internal/model"
)

// Orchestrator runs a single job through gitcache -> codex -> gitea.
type Orchestrator struct {
	Store   model.Store
	Cache   model.GitCache
	Codex   model.CodexRunner
	Gitea   model.GiteaClient
	BaseRef string // unused; per-PR base comes from the event

	TriggerKeywords []string
	RepoAllowlist   []string // entries "owner/repo"; empty = allow all
	Logger          *log.Logger
}

func (o *Orchestrator) logf(format string, args ...any) {
	if o.Logger != nil {
		o.Logger.Printf(format, args...)
	}
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
	if len(o.RepoAllowlist) == 0 {
		return true
	}
	full := pr.Owner + "/" + pr.Repo
	for _, a := range o.RepoAllowlist {
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

// review runs a full (new) or incremental (resume) review and posts it.
func (o *Orchestrator) review(ctx context.Context, jobID int64, ev *model.WebhookEvent, isUpdate bool) error {
	pr := ev.PR

	// Prior state (session continuity).
	o.jobLog(ctx, jobID, "state", "loading prior PR session")
	prev, err := o.Store.GetPull(ctx, pr)
	if err != nil {
		return fmt.Errorf("get pull: %w", err)
	}
	sessionID := ""
	prevHead := ""
	var prevReviewID int64
	if prev != nil {
		sessionID = prev.SessionID
		prevHead = prev.HeadSHA
		prevReviewID = prev.LastReviewID
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

	o.jobLog(ctx, jobID, "gitea", "fetching PR diff")
	diff, err := o.Gitea.GetDiff(ctx, pr)
	if err != nil {
		return fmt.Errorf("get diff: %w", err)
	}
	o.jobLog(ctx, jobID, "gitea", "diff fetched")

	in := model.CodexInput{
		Worktree: worktree,
		BaseRef:  ev.BaseRef,
	}
	if isUpdate && sessionID != "" {
		in.SessionID = sessionID
		in.Note = fmt.Sprintf("PR head 已从 %s 更新到 %s。请基于 %s 重新审查当前 diff，并在摘要中说明之前报告的问题哪些已经修复。", shortSHA(prevHead), shortSHA(ev.HeadSHA), ev.BaseRef)
	}

	o.jobLog(ctx, jobID, "codex", "review started")
	result, err := o.Codex.Review(ctx, in)
	if err != nil {
		return fmt.Errorf("codex review: %w", err)
	}
	o.jobLog(ctx, jobID, "codex", fmt.Sprintf("review finished: %d findings, session=%s", len(result.Findings), result.SessionID))

	// Persist PR state + session id.
	o.jobLog(ctx, jobID, "state", "saving PR session")
	st := &model.PullState{PR: pr, SessionID: result.SessionID, HeadSHA: ev.HeadSHA, BaseRef: ev.BaseRef}
	if st.SessionID == "" {
		st.SessionID = sessionID // keep prior if codex returned none
	}
	if err := o.Store.UpsertPull(ctx, st); err != nil {
		o.logf("upsert pull: %v", err)
	}
	pullRow, _ := o.Store.GetPull(ctx, pr)

	// Map findings to inline comments; fold unmappable ones into the summary.
	comments, unmapped := mapFindings(diff, result.Findings)

	// Dismiss our previous review so stale comments don't pile up.
	if isUpdate && prevReviewID != 0 {
		o.jobLog(ctx, jobID, "gitea", "dismissing previous review")
		if err := o.Gitea.DismissReview(ctx, pr, prevReviewID, "新提交触发了重新审查，此前审查已被替代。"); err != nil {
			o.logf("dismiss prior review %d: %v", prevReviewID, err)
		}
	}

	body := buildSummary(result, unmapped)
	event := model.ReviewEventComment
	if result.OverallSeverity.Blocking() || anyBlocking(result.Findings) {
		event = model.ReviewEventRequestChanges
	}

	o.jobLog(ctx, jobID, "gitea", fmt.Sprintf("posting review: %d inline comments, verdict=%s", len(comments), event))
	reviewID, err := o.Gitea.PostReview(ctx, pr, ev.HeadSHA, event, body, comments)
	if err != nil {
		return fmt.Errorf("post review: %w", err)
	}
	o.jobLog(ctx, jobID, "gitea", fmt.Sprintf("review posted: id=%d", reviewID))

	if pullRow != nil {
		if err := o.Store.SaveFindings(ctx, pullRow.ID, ev.HeadSHA, result.Findings); err != nil {
			o.logf("save findings: %v", err)
		}
	}
	if err := o.Store.SetLastReview(ctx, pr, reviewID); err != nil {
		o.logf("set last review: %v", err)
	}

	o.logf("reviewed %s/%s#%d: %d findings (%d inline), verdict=%s, session=%s",
		pr.Owner, pr.Repo, pr.Number, len(result.Findings), len(comments), event, result.SessionID)
	return nil
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
	prev, err := o.Store.GetPull(ctx, ev.PR)
	if err != nil {
		return fmt.Errorf("get pull: %w", err)
	}
	if prev == nil || prev.SessionID == "" {
		o.jobLog(ctx, jobID, "gitea", "posting missing-session comment")
		_, perr := o.Gitea.PostComment(ctx, ev.PR, "我还没有审查过这个 PR，因此没有可继续的会话。请先推送一次提交或重新打开 PR 来触发审查。")
		return perr
	}

	o.jobLog(ctx, jobID, "git", "preparing worktree")
	worktree, err := o.Cache.Prepare(ctx, ev.PR, ev.CloneURL, prev.BaseRef, ev.HeadRef, prev.HeadSHA)
	if err != nil {
		return fmt.Errorf("git prepare: %w", err)
	}
	defer o.Cache.Cleanup(ev.PR)
	o.jobLog(ctx, jobID, "git", "worktree ready")

	o.jobLog(ctx, jobID, "codex", "answer started")
	answer, err := o.Codex.Ask(ctx, prev.SessionID, worktree, question)
	if err != nil {
		return fmt.Errorf("codex ask: %w", err)
	}
	o.jobLog(ctx, jobID, "codex", "answer finished")
	o.jobLog(ctx, jobID, "gitea", "posting answer comment")
	_, err = o.Gitea.PostComment(ctx, ev.PR, answer)
	return err
}

// matchTrigger returns the text following a trigger keyword, if present.
func (o *Orchestrator) matchTrigger(body string) (string, bool) {
	for _, kw := range o.TriggerKeywords {
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

// ---------- helpers ----------

func mapFindings(diff model.DiffMap, fs []model.Finding) (comments []model.ReviewComment, unmapped []model.Finding) {
	for _, f := range fs {
		newPos, oldPos, ok := diff.Position(f.Path, f.Line, f.Side)
		if !ok {
			unmapped = append(unmapped, f)
			continue
		}
		comments = append(comments, model.ReviewComment{
			Path:        f.Path,
			Body:        formatFinding(f),
			NewPosition: newPos,
			OldPosition: oldPos,
		})
	}
	return comments, unmapped
}

func formatFinding(f model.Finding) string {
	return fmt.Sprintf("**[%s] %s**\n\n%s", strings.ToUpper(string(f.Severity)), f.Title, f.Body)
}

func buildSummary(r *model.ReviewResult, unmapped []model.Finding) string {
	var b strings.Builder
	b.WriteString("## Codex 审查\n\n")
	b.WriteString(r.Summary)
	b.WriteString(fmt.Sprintf("\n\n整体严重程度：**%s**\n", r.OverallSeverity))
	if len(unmapped) > 0 {
		b.WriteString("\n### 无法映射到 diff 行的问题\n\n")
		for _, f := range unmapped {
			b.WriteString(fmt.Sprintf("- **[%s] %s** (`%s:%d`): %s\n",
				strings.ToUpper(string(f.Severity)), f.Title, f.Path, f.Line, f.Body))
		}
	}
	b.WriteString("\n_仅静态审查，未构建或执行代码。_")
	return b.String()
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
