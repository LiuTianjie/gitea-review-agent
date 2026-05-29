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

// Process decodes a job and dispatches by event type/action.
func (o *Orchestrator) Process(ctx context.Context, job *model.Job) error {
	var ev model.WebhookEvent
	if err := json.Unmarshal(job.Payload, &ev); err != nil {
		return fmt.Errorf("decode payload: %w", err)
	}

	if !o.allowed(ev.PR) {
		o.logf("skip %s/%s#%d: repo not in allowlist", ev.PR.Owner, ev.PR.Repo, ev.PR.Number)
		return nil
	}

	switch ev.Event {
	case model.EventPullRequest:
		return o.handlePR(ctx, &ev)
	case model.EventIssueComment:
		return o.handleComment(ctx, &ev)
	default:
		o.logf("ignoring event type %q", ev.Event)
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

func (o *Orchestrator) handlePR(ctx context.Context, ev *model.WebhookEvent) error {
	switch ev.Action {
	case "opened", "reopened":
		return o.review(ctx, ev, false)
	case "synchronized":
		return o.review(ctx, ev, true)
	case "closed":
		if err := o.Cache.Cleanup(ev.PR); err != nil {
			o.logf("cleanup on close %s#%d: %v", ev.PR.Repo, ev.PR.Number, err)
		}
		return nil
	default:
		o.logf("ignoring PR action %q", ev.Action)
		return nil
	}
}

// review runs a full (new) or incremental (resume) review and posts it.
func (o *Orchestrator) review(ctx context.Context, ev *model.WebhookEvent, isUpdate bool) error {
	pr := ev.PR

	// Prior state (session continuity).
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

	worktree, err := o.Cache.Prepare(ctx, pr, ev.CloneURL, ev.BaseRef, ev.HeadRef, ev.HeadSHA)
	if err != nil {
		return fmt.Errorf("git prepare: %w", err)
	}
	defer func() {
		if cerr := o.Cache.Cleanup(pr); cerr != nil {
			o.logf("cleanup %s#%d: %v", pr.Repo, pr.Number, cerr)
		}
	}()

	diff, err := o.Gitea.GetDiff(ctx, pr)
	if err != nil {
		return fmt.Errorf("get diff: %w", err)
	}

	in := model.CodexInput{
		Worktree: worktree,
		BaseRef:  ev.BaseRef,
	}
	if isUpdate && sessionID != "" {
		in.SessionID = sessionID
		in.Note = fmt.Sprintf("The PR head moved from %s to %s. Re-review the current diff against %s and note which previously-reported issues are now fixed.", shortSHA(prevHead), shortSHA(ev.HeadSHA), ev.BaseRef)
	}

	result, err := o.Codex.Review(ctx, in)
	if err != nil {
		return fmt.Errorf("codex review: %w", err)
	}

	// Persist PR state + session id.
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
		if err := o.Gitea.DismissReview(ctx, pr, prevReviewID, "Superseded by re-review after new commits."); err != nil {
			o.logf("dismiss prior review %d: %v", prevReviewID, err)
		}
	}

	body := buildSummary(result, unmapped)
	event := model.ReviewEventComment
	if result.OverallSeverity.Blocking() || anyBlocking(result.Findings) {
		event = model.ReviewEventRequestChanges
	}

	reviewID, err := o.Gitea.PostReview(ctx, pr, ev.HeadSHA, event, body, comments)
	if err != nil {
		return fmt.Errorf("post review: %w", err)
	}

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
func (o *Orchestrator) handleComment(ctx context.Context, ev *model.WebhookEvent) error {
	if !ev.IsPR || ev.Action != "created" {
		return nil
	}
	question, ok := o.matchTrigger(ev.CommentBody)
	if !ok {
		return nil
	}

	prev, err := o.Store.GetPull(ctx, ev.PR)
	if err != nil {
		return fmt.Errorf("get pull: %w", err)
	}
	if prev == nil || prev.SessionID == "" {
		_, perr := o.Gitea.PostComment(ctx, ev.PR, "I haven't reviewed this PR yet, so I have no session to answer from. Push a commit or reopen the PR to trigger a review first.")
		return perr
	}

	worktree, err := o.Cache.Prepare(ctx, ev.PR, ev.CloneURL, prev.BaseRef, ev.HeadRef, prev.HeadSHA)
	if err != nil {
		return fmt.Errorf("git prepare: %w", err)
	}
	defer o.Cache.Cleanup(ev.PR)

	answer, err := o.Codex.Ask(ctx, prev.SessionID, worktree, question)
	if err != nil {
		return fmt.Errorf("codex ask: %w", err)
	}
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
				rest = "Please re-review the current diff and answer any open questions."
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
	b.WriteString("## Codex review\n\n")
	b.WriteString(r.Summary)
	b.WriteString(fmt.Sprintf("\n\nOverall severity: **%s**\n", r.OverallSeverity))
	if len(unmapped) > 0 {
		b.WriteString("\n### Findings outside the diff\n\n")
		for _, f := range unmapped {
			b.WriteString(fmt.Sprintf("- **[%s] %s** (`%s:%d`): %s\n",
				strings.ToUpper(string(f.Severity)), f.Title, f.Path, f.Line, f.Body))
		}
	}
	b.WriteString("\n_Static review only — code was not built or executed._")
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
