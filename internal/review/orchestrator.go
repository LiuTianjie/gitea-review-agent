// Package review wires the modules into the end-to-end review pipeline.
package review

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
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
	Logger          *log.Logger
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

	if len(o.reviewers()) == 0 {
		return fmt.Errorf("no reviewers configured")
	}

	o.jobLog(ctx, jobID, "state", "loading PR state")
	pull, err := o.Store.GetPull(ctx, pr)
	if err != nil {
		return fmt.Errorf("get pull: %w", err)
	}
	if pull == nil {
		if err := o.Store.UpsertPull(ctx, &model.PullState{PR: pr, HeadSHA: ev.HeadSHA, BaseRef: ev.BaseRef}); err != nil {
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

	o.jobLog(ctx, jobID, "gitea", "fetching PR diff")
	diff, err := o.Gitea.GetDiff(ctx, pr)
	if err != nil {
		return fmt.Errorf("get diff: %w", err)
	}
	o.jobLog(ctx, jobID, "gitea", "diff fetched")

	var errs []error
	successes := 0
	for _, reviewer := range o.reviewers() {
		if err := o.runReviewer(ctx, jobID, ev, isUpdate, pull.ID, worktree, diff, reviewer); err != nil {
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

func (o *Orchestrator) runReviewer(ctx context.Context, jobID int64, ev *model.WebhookEvent, isUpdate bool, pullID int64, worktree string, diff model.DiffMap, reviewer model.Reviewer) error {
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
	o.jobLog(ctx, jobID, "gitea", fmt.Sprintf("posting %s review: %d inline comments, verdict=%s", agent, len(comments), event))
	reviewID, err := o.Gitea.PostReview(ctx, pr, ev.HeadSHA, event, body, comments)
	if err != nil {
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
	reviewer := o.primaryReviewer()
	if reviewer == nil {
		return fmt.Errorf("no reviewer configured")
	}
	prev, err := o.Store.GetReviewerState(ctx, ev.PR, reviewer.Name())
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

	o.jobLog(ctx, jobID, reviewer.Name(), "answer started")
	answer, err := reviewer.Ask(ctx, prev.SessionID, worktree, question)
	if err != nil {
		return fmt.Errorf("%s ask: %w", reviewer.Name(), err)
	}
	o.jobLog(ctx, jobID, reviewer.Name(), "answer finished")
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
	return fmt.Sprintf("**[%s] %s**\n\n%s", strings.ToUpper(string(f.Severity)), f.Title, f.Body)
}

func buildSummary(agent string, r *model.ReviewResult, unmapped []model.Finding) string {
	var b strings.Builder
	b.WriteString("## ")
	b.WriteString(reviewerDisplayName(agent))
	b.WriteString(" 审查\n\n")
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

func reviewerDisplayName(agent string) string {
	switch normalizeAgentName(agent) {
	case "codex":
		return "Codex"
	case "claude":
		return "Claude"
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
