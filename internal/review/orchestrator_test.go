package review

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/turning4th/codex-gitea/internal/model"
)

// ---------- fakes ----------

type fakeStore struct {
	mu             sync.Mutex
	pulls          map[string]*model.PullState
	findings       map[int64][]model.Finding
	storedFindings map[int64][]model.StoredFinding
	fixed          map[int64][]string
	lastRev        map[string]int64
	reviewRuns     []model.ReviewRun
	reviewerStates map[string]*model.PullReviewerState
}

func newFakeStore() *fakeStore {
	return &fakeStore{
		pulls:          map[string]*model.PullState{},
		findings:       map[int64][]model.Finding{},
		storedFindings: map[int64][]model.StoredFinding{},
		fixed:          map[int64][]string{},
		lastRev:        map[string]int64{},
		reviewerStates: map[string]*model.PullReviewerState{},
	}
}

func pk(pr model.PRRef) string                { return pr.Key() }
func rpk(pr model.PRRef, agent string) string { return pr.Key() + ":" + agent }

func (s *fakeStore) GetPull(_ context.Context, pr model.PRRef) (*model.PullState, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if st, ok := s.pulls[pk(pr)]; ok {
		cp := *st
		return &cp, nil
	}
	return nil, nil
}

func (s *fakeStore) UpsertPull(_ context.Context, st *model.PullState) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	cp := *st
	if cp.ID == 0 {
		cp.ID = int64(len(s.pulls) + 1)
	}
	if existing, ok := s.pulls[pk(st.PR)]; ok {
		cp.ID = existing.ID
		cp.LastReviewID = existing.LastReviewID
	}
	s.pulls[pk(st.PR)] = &cp
	return nil
}

func (s *fakeStore) SetSession(_ context.Context, pr model.PRRef, id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if st, ok := s.pulls[pk(pr)]; ok {
		st.SessionID = id
	}
	return nil
}

func (s *fakeStore) SetLastReview(_ context.Context, pr model.PRRef, id int64) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.lastRev[pk(pr)] = id
	if st, ok := s.pulls[pk(pr)]; ok {
		st.LastReviewID = id
	}
	return nil
}

func (s *fakeStore) GetReviewerState(_ context.Context, pr model.PRRef, agent string) (*model.PullReviewerState, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if st, ok := s.reviewerStates[rpk(pr, agent)]; ok {
		cp := *st
		return &cp, nil
	}
	if st, ok := s.pulls[pk(pr)]; ok {
		return &model.PullReviewerState{
			ID: st.ID, PullID: st.ID, PR: pr, Agent: agent,
			SessionID: st.SessionID, HeadSHA: st.HeadSHA, BaseRef: st.BaseRef, LastReviewID: st.LastReviewID,
		}, nil
	}
	return nil, nil
}

func (s *fakeStore) UpsertReviewerState(_ context.Context, st *model.PullReviewerState) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	cp := &model.PullState{
		ID: st.PullID, PR: st.PR, SessionID: st.SessionID, HeadSHA: st.HeadSHA,
		BaseRef: st.BaseRef, LastReviewID: st.LastReviewID,
	}
	if cp.ID == 0 {
		cp.ID = int64(len(s.pulls) + 1)
	}
	rs := *st
	rs.PullID = cp.ID
	s.reviewerStates[rpk(st.PR, st.Agent)] = &rs
	if st.Agent == "" || st.Agent == "codex" {
		s.pulls[pk(st.PR)] = cp
	}
	return nil
}

func (s *fakeStore) CreateReviewRun(_ context.Context, run *model.ReviewRun) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	run.ID = int64(len(s.reviewRuns) + 1)
	s.reviewRuns = append(s.reviewRuns, *run)
	return nil
}

func (s *fakeStore) FinishReviewRun(context.Context, int64, model.ReviewRunStatus, string, int) error {
	return nil
}

func (s *fakeStore) SaveFindings(_ context.Context, pullID int64, _ string, fs []model.Finding, _ model.FindingSaveOptions) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.findings[pullID] = fs
	stored := make([]model.StoredFinding, 0, len(fs))
	for i, f := range fs {
		stored = append(stored, model.StoredFinding{
			Finding:     f,
			ID:          int64(i + 1),
			PullID:      pullID,
			Fingerprint: f.Fingerprint(),
			Status:      "open",
		})
	}
	s.storedFindings[pullID] = stored
	return nil
}

func (s *fakeStore) MarkFindingsFixed(_ context.Context, pullID int64, fingerprints []string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.fixed[pullID] = append(s.fixed[pullID], fingerprints...)
	status := stringSet(fingerprints)
	for i := range s.storedFindings[pullID] {
		if status[s.storedFindings[pullID][i].Fingerprint] {
			s.storedFindings[pullID][i].Status = "fixed"
		}
	}
	return nil
}

// unused interface methods
func (s *fakeStore) EnqueueJob(context.Context, *model.WebhookEvent) (*model.Job, bool, error) {
	return nil, false, nil
}
func (s *fakeStore) SupersedePending(context.Context, model.PRRef) error             { return nil }
func (s *fakeStore) ClaimJob(context.Context) (*model.Job, error)                    { return nil, nil }
func (s *fakeStore) FinishJob(context.Context, int64, model.JobStatus, string) error { return nil }
func (s *fakeStore) RecoverRunning(context.Context) error                            { return nil }
func (s *fakeStore) AppendJobLog(context.Context, int64, string, string) error       { return nil }
func (s *fakeStore) ListJobs(context.Context, int, int) ([]model.JobView, error)     { return nil, nil }
func (s *fakeStore) ListJobsFiltered(context.Context, model.JobFilter, int, int) ([]model.JobView, error) {
	return nil, nil
}
func (s *fakeStore) CountJobs(context.Context) (int, error)                          { return 0, nil }
func (s *fakeStore) CountJobsFiltered(context.Context, model.JobFilter) (int, error) { return 0, nil }
func (s *fakeStore) JobStats(context.Context) (model.JobStats, error)                { return model.JobStats{}, nil }
func (s *fakeStore) JobStatsFiltered(context.Context, model.JobFilter) (model.JobStats, error) {
	return model.JobStats{}, nil
}
func (s *fakeStore) GetJob(context.Context, int64) (*model.Job, error) { return nil, nil }
func (s *fakeStore) ListFindings(_ context.Context, pullID int64) ([]model.StoredFinding, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	got := s.storedFindings[pullID]
	out := make([]model.StoredFinding, len(got))
	copy(out, got)
	return out, nil
}
func (s *fakeStore) MarkFindingPosted(context.Context, int64, int64, int64) error { return nil }
func (s *fakeStore) GetSetting(context.Context, string) (string, bool, error)     { return "", false, nil }
func (s *fakeStore) SetSetting(context.Context, string, string, bool) error       { return nil }
func (s *fakeStore) AllSettings(context.Context) (map[string]string, error)       { return nil, nil }
func (s *fakeStore) CreateAnalysisReport(context.Context, model.AnalysisSummary) (*model.AnalysisReport, error) {
	return nil, nil
}
func (s *fakeStore) LatestAnalysisReport(context.Context) (*model.AnalysisReport, error) {
	return nil, nil
}
func (s *fakeStore) ListAnalysisReports(context.Context, int) ([]model.AnalysisReport, error) {
	return nil, nil
}
func (s *fakeStore) BuildAnalysisSummary(context.Context) (model.AnalysisSummary, error) {
	return model.AnalysisSummary{}, nil
}
func (s *fakeStore) Close() error { return nil }

type fakeCache struct {
	prepared int
	cleaned  int
	worktree string
}

func (c *fakeCache) Prepare(_ context.Context, _ model.PRRef, _, _, _, _ string) (string, error) {
	c.prepared++
	if c.worktree != "" {
		return c.worktree, nil
	}
	return "/work/fake", nil
}
func (c *fakeCache) Cleanup(model.PRRef) error { c.cleaned++; return nil }

type fakeCodex struct {
	name     string
	result   *model.ReviewResult
	err      error
	lastIn   model.CodexInput
	askReply string
	lastAsk  string
}

func (c *fakeCodex) Name() string {
	if c.name != "" {
		return c.name
	}
	return "codex"
}
func (c *fakeCodex) Review(_ context.Context, in model.CodexInput) (*model.ReviewResult, error) {
	c.lastIn = in
	if c.err != nil {
		return nil, c.err
	}
	return c.result, nil
}
func (c *fakeCodex) Ask(_ context.Context, _, _, q string) (string, error) {
	c.lastAsk = q
	return c.askReply, nil
}
func (c *fakeCodex) Status(context.Context) (string, error) { return "ok", nil }

type fakeGitea struct {
	diff         model.DiffMap
	diffCalls    int
	comments     []model.PullComment
	commentCalls int
	lastEvent    model.ReviewEventType
	lastComments []model.ReviewComment
	dismissed    []int64
	posted       []string
	reviewID     int64
	reviews      []model.ReviewEventType
	prStatus     model.PullRequestStatus
	postErr      error
}

func (g *fakeGitea) GetPullRequestStatus(context.Context, model.PRRef) (model.PullRequestStatus, error) {
	if g.prStatus.State == "" && !g.prStatus.Merged {
		return model.PullRequestStatus{State: "open"}, nil
	}
	return g.prStatus, nil
}
func (g *fakeGitea) GetDiff(context.Context, model.PRRef) (model.DiffMap, error) {
	g.diffCalls++
	return g.diff, nil
}
func (g *fakeGitea) ListIssueComments(context.Context, model.PRRef) ([]model.PullComment, error) {
	g.commentCalls++
	return g.comments, nil
}
func (g *fakeGitea) PostReview(_ context.Context, _ model.PRRef, _ string, ev model.ReviewEventType, _ string, cs []model.ReviewComment) (int64, error) {
	if g.postErr != nil {
		return 0, g.postErr
	}
	g.lastEvent = ev
	g.lastComments = cs
	g.reviews = append(g.reviews, ev)
	g.reviewID++
	return g.reviewID, nil
}
func (g *fakeGitea) PostComment(_ context.Context, _ model.PRRef, body string) (int64, error) {
	g.posted = append(g.posted, body)
	return 1, nil
}
func (g *fakeGitea) ListReviews(context.Context, model.PRRef) ([]model.GiteaReview, error) {
	return nil, nil
}
func (g *fakeGitea) DismissReview(_ context.Context, _ model.PRRef, id int64, _ string) error {
	g.dismissed = append(g.dismissed, id)
	return nil
}

// ---------- helpers ----------

func runTestGit(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
	}
}

func diffWith(path string, lines ...int) model.DiffMap {
	nl := map[int]bool{}
	for _, l := range lines {
		nl[l] = true
	}
	return model.DiffMap{Files: map[string]model.FileDiff{
		path: {NewLines: nl, OldLines: map[int]bool{}},
	}}
}

func testDiffRepo(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	runTestGit(t, dir, "init", "-b", "main")
	runTestGit(t, dir, "config", "user.email", "bot@example.com")
	runTestGit(t, dir, "config", "user.name", "Bot")
	if err := os.WriteFile(filepath.Join(dir, "base.txt"), []byte("base\n"), 0o644); err != nil {
		t.Fatalf("write base: %v", err)
	}
	runTestGit(t, dir, "add", ".")
	runTestGit(t, dir, "commit", "-m", "base")
	runTestGit(t, dir, "checkout", "-b", "feature")
	for _, name := range []string{"a.txt", "b.txt"} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(name+"\n"), 0o644); err != nil {
			t.Fatalf("write %s: %v", name, err)
		}
	}
	runTestGit(t, dir, "add", ".")
	runTestGit(t, dir, "commit", "-m", "feature")
	return dir
}

func prEvent(action, head string) *model.Job {
	ev := model.WebhookEvent{
		Event: model.EventPullRequest, Action: action,
		PR:      model.PRRef{Owner: "alice", Repo: "repo", Number: 7},
		BaseRef: "main", HeadRef: "feat", HeadSHA: head, CloneURL: "file:///x",
	}
	b, _ := json.Marshal(ev)
	return &model.Job{Payload: b, PR: ev.PR, Event: ev.Event, Action: action}
}

// ---------- tests ----------

func TestInspectWorktreeDiffUsesBaseToHeadThreeDotDiff(t *testing.T) {
	dir := testDiffRepo(t)

	summary, err := inspectWorktreeDiff(context.Background(), dir, "main")
	if err != nil {
		t.Fatalf("inspectWorktreeDiff: %v", err)
	}
	if summary.BaseRef != "main" || summary.FileCount != 2 {
		t.Fatalf("summary = %+v, want base main and 2 files", summary)
	}
	if len(summary.Files) != 2 || !stringSet(summary.Files)["a.txt"] || !stringSet(summary.Files)["b.txt"] {
		t.Fatalf("summary files = %+v, want a.txt and b.txt", summary.Files)
	}
	if len(summary.Numstat) != 2 || !strings.Contains(strings.Join(summary.Numstat, "\n"), "a.txt") {
		t.Fatalf("summary numstat = %+v, want entries for changed files", summary.Numstat)
	}
	formatted := formatWorktreeDiffSummary(summary)
	if !strings.Contains(formatted, "base=main") || !strings.Contains(formatted, "files=2") {
		t.Fatalf("formatted summary missing key data: %s", formatted)
	}
	note := appendDiffInventoryNote("existing context", summary)
	if !strings.Contains(note, "Current PR diff inventory") ||
		!strings.Contains(note, "changed_files: 2") ||
		!strings.Contains(note, "a.txt") ||
		!strings.Contains(note, "existing context") {
		t.Fatalf("inventory note missing expected coverage data:\n%s", note)
	}
}

func TestReview_OpenedPassesDiffInventoryNote(t *testing.T) {
	st := newFakeStore()
	cx := &fakeCodex{result: &model.ReviewResult{
		Summary: "clean", OverallSeverity: model.Severity("none"), SessionID: "thread-1",
	}}
	gt := &fakeGitea{diff: diffWith("a.txt", 1)}
	o := &Orchestrator{
		Store: st, Cache: &fakeCache{worktree: testDiffRepo(t)}, Reviewers: []model.Reviewer{cx}, Gitea: gt,
	}

	if err := o.Process(context.Background(), prEvent("opened", "head123")); err != nil {
		t.Fatalf("Process: %v", err)
	}
	if !strings.Contains(cx.lastIn.Note, "Current PR diff inventory") ||
		!strings.Contains(cx.lastIn.Note, "changed_files: 2") ||
		!strings.Contains(cx.lastIn.Note, "a.txt") ||
		!strings.Contains(cx.lastIn.Note, "b.txt") {
		t.Fatalf("reviewer note missing diff inventory:\n%s", cx.lastIn.Note)
	}
}

func TestReview_Opened_InlineAndRequestChanges(t *testing.T) {
	st := newFakeStore()
	gc := &fakeCache{}
	cx := &fakeCodex{result: &model.ReviewResult{
		Summary: "found stuff", OverallSeverity: model.SeverityHigh, SessionID: "thread-1",
		Findings: []model.Finding{
			{Path: "calc.go", Line: 19, Side: model.SideNew, Severity: model.SeverityHigh, Title: "div by zero", Body: "boom"},
			{Path: "calc.go", Line: 999, Side: model.SideNew, Severity: model.SeverityLow, Title: "off diff", Body: "nope"},
		},
	}}
	gt := &fakeGitea{diff: diffWith("calc.go", 19)}
	o := &Orchestrator{Store: st, Cache: gc, Reviewers: []model.Reviewer{cx}, Gitea: gt}

	if err := o.Process(context.Background(), prEvent("opened", "aaa111")); err != nil {
		t.Fatalf("Process: %v", err)
	}
	if gt.lastEvent != model.ReviewEventRequestChanges {
		t.Errorf("event = %s, want REQUEST_CHANGES (has high finding)", gt.lastEvent)
	}
	if len(gt.lastComments) != 1 || gt.lastComments[0].NewPosition != 19 {
		t.Errorf("want 1 inline comment at line 19, got %+v", gt.lastComments)
	}
	// session persisted
	if pull := st.pulls[pk(prEvent("opened", "x").PR)]; pull == nil || pull.SessionID != "thread-1" {
		t.Errorf("session not persisted: %+v", pull)
	}
	if gc.cleaned == 0 {
		t.Error("worktree not cleaned up")
	}
}

func TestReview_TwoReviewersSharePreparedWorktreeAndDiff(t *testing.T) {
	st := newFakeStore()
	gc := &fakeCache{}
	codexReviewer := &fakeCodex{name: "codex", result: &model.ReviewResult{
		Summary: "codex ok", OverallSeverity: model.Severity("none"), SessionID: "codex-thread",
	}}
	claudeReviewer := &fakeCodex{name: "claude", result: &model.ReviewResult{
		Summary: "claude found", OverallSeverity: model.SeverityHigh, SessionID: "claude-thread",
		Findings: []model.Finding{{
			Path: "calc.go", Line: 19, Side: model.SideNew,
			Severity: model.SeverityHigh, Title: "bad math", Body: "boom",
		}},
	}}
	gt := &fakeGitea{
		diff: diffWith("calc.go", 19),
		comments: []model.PullComment{{
			User: "zijiaw",
			Body: "@bot-tifenxia 对照 agents.md 整体审视接口实现，检查是否符合开发规范",
		}},
	}
	o := &Orchestrator{Store: st, Cache: gc, Reviewers: []model.Reviewer{codexReviewer, claudeReviewer}, Gitea: gt}

	if err := o.Process(context.Background(), prEvent("opened", "aaa111")); err != nil {
		t.Fatalf("Process: %v", err)
	}
	if gc.prepared != 1 {
		t.Fatalf("Prepare calls = %d, want 1", gc.prepared)
	}
	if gt.diffCalls != 1 {
		t.Fatalf("GetDiff calls = %d, want 1", gt.diffCalls)
	}
	if gt.commentCalls != 1 {
		t.Fatalf("ListIssueComments calls = %d, want 1", gt.commentCalls)
	}
	if codexReviewer.lastIn.Worktree == "" || codexReviewer.lastIn.Worktree != claudeReviewer.lastIn.Worktree {
		t.Fatalf("reviewers did not share worktree: codex=%q claude=%q", codexReviewer.lastIn.Worktree, claudeReviewer.lastIn.Worktree)
	}
	for _, reviewer := range []*fakeCodex{codexReviewer, claudeReviewer} {
		if !strings.Contains(reviewer.lastIn.Note, "PR discussion context") ||
			!strings.Contains(reviewer.lastIn.Note, "agents.md") {
			t.Fatalf("%s reviewer missing PR comment context: %q", reviewer.Name(), reviewer.lastIn.Note)
		}
	}
	if len(gt.reviews) != 2 {
		t.Fatalf("posted reviews = %d, want 2", len(gt.reviews))
	}
	if gt.reviews[1] != model.ReviewEventRequestChanges {
		t.Fatalf("claude review event = %s, want REQUEST_CHANGES", gt.reviews[1])
	}
}

func TestReview_SkipsWhenPullRequestAlreadyClosed(t *testing.T) {
	st := newFakeStore()
	gc := &fakeCache{}
	cx := &fakeCodex{result: &model.ReviewResult{Summary: "unused", OverallSeverity: model.Severity("none")}}
	gt := &fakeGitea{prStatus: model.PullRequestStatus{State: "closed"}}
	o := &Orchestrator{Store: st, Cache: gc, Reviewers: []model.Reviewer{cx}, Gitea: gt}

	if err := o.Process(context.Background(), prEvent("opened", "aaa111")); err != nil {
		t.Fatalf("Process: %v", err)
	}
	if gc.prepared != 0 {
		t.Fatalf("Prepare calls = %d, want 0", gc.prepared)
	}
	if cx.lastIn.Worktree != "" {
		t.Fatalf("reviewer was invoked for closed PR: %+v", cx.lastIn)
	}
	if len(gt.reviews) != 0 {
		t.Fatalf("posted reviews = %d, want 0", len(gt.reviews))
	}
}

func TestReview_ClosedDuringPostStopsRemainingReviewers(t *testing.T) {
	st := newFakeStore()
	gc := &fakeCache{}
	codexReviewer := &fakeCodex{name: "codex", result: &model.ReviewResult{
		Summary: "codex found", OverallSeverity: model.SeverityHigh, SessionID: "codex-thread",
		Findings: []model.Finding{{
			Path: "calc.go", Line: 19, Side: model.SideNew,
			Severity: model.SeverityHigh, Title: "bad math", Body: "boom",
		}},
	}}
	claudeReviewer := &fakeCodex{name: "claude", result: &model.ReviewResult{
		Summary: "claude should not run", OverallSeverity: model.Severity("none"),
	}}
	gt := &fakeGitea{
		diff:    diffWith("calc.go", 19),
		postErr: errors.New(`gitea: POST /api/v1/repos/alice/repo/pulls/7/reviews: status 422: {"message":"can't submit review for a closed or merged PR"}`),
	}
	o := &Orchestrator{Store: st, Cache: gc, Reviewers: []model.Reviewer{codexReviewer, claudeReviewer}, Gitea: gt}

	if err := o.Process(context.Background(), prEvent("opened", "aaa111")); err != nil {
		t.Fatalf("Process: %v", err)
	}
	if codexReviewer.lastIn.Worktree == "" {
		t.Fatal("codex reviewer was not invoked")
	}
	if claudeReviewer.lastIn.Worktree != "" {
		t.Fatalf("claude reviewer should not run after closed PR post failure: %+v", claudeReviewer.lastIn)
	}
	if len(gt.reviews) != 0 {
		t.Fatalf("posted reviews = %d, want 0", len(gt.reviews))
	}
}

func TestReview_OneReviewerFailureDoesNotFailJobWhenAnotherSucceeds(t *testing.T) {
	st := newFakeStore()
	failing := &fakeCodex{name: "claude", err: errors.New("relay unavailable")}
	working := &fakeCodex{name: "codex", result: &model.ReviewResult{
		Summary: "codex ok", OverallSeverity: model.Severity("none"), SessionID: "codex-thread",
	}}
	gt := &fakeGitea{diff: diffWith("calc.go", 19)}
	o := &Orchestrator{
		Store: st, Cache: &fakeCache{},
		Reviewers: []model.Reviewer{failing, working}, Gitea: gt,
	}

	if err := o.Process(context.Background(), prEvent("opened", "aaa111")); err != nil {
		t.Fatalf("Process should not fail when one reviewer succeeds: %v", err)
	}
	if len(gt.reviews) != 1 {
		t.Fatalf("posted reviews = %d, want 1 successful reviewer review", len(gt.reviews))
	}
	if len(st.reviewRuns) != 2 {
		t.Fatalf("review runs = %d, want 2", len(st.reviewRuns))
	}
}

func TestReview_Synchronized_ResumesAndDismisses(t *testing.T) {
	st := newFakeStore()
	// seed prior review state
	st.pulls["alice__repo"] = &model.PullState{
		ID: 1, PR: model.PRRef{Owner: "alice", Repo: "repo", Number: 7},
		SessionID: "thread-1", HeadSHA: "old999", BaseRef: "main", LastReviewID: 42,
	}
	gc := &fakeCache{}
	cx := &fakeCodex{result: &model.ReviewResult{
		Summary: "rechecked", OverallSeverity: model.SeverityLow, SessionID: "thread-1",
	}}
	gt := &fakeGitea{diff: diffWith("calc.go", 19)}
	o := &Orchestrator{Store: st, Cache: gc, Reviewers: []model.Reviewer{cx}, Gitea: gt}

	if err := o.Process(context.Background(), prEvent("synchronized", "new222")); err != nil {
		t.Fatalf("Process: %v", err)
	}
	if cx.lastIn.SessionID != "thread-1" {
		t.Errorf("resume not used: SessionID=%q", cx.lastIn.SessionID)
	}
	if cx.lastIn.Note == "" {
		t.Error("resume note empty; expected head-moved context")
	}
	if len(gt.dismissed) != 1 || gt.dismissed[0] != 42 {
		t.Errorf("prior review not dismissed: %v", gt.dismissed)
	}
	if gt.lastEvent != model.ReviewEventComment {
		t.Errorf("event = %s, want COMMENT (low severity)", gt.lastEvent)
	}
}

func TestReview_Synchronized_CarriesForwardPriorOpenFindings(t *testing.T) {
	st := newFakeStore()
	pr := model.PRRef{Owner: "alice", Repo: "repo", Number: 7}
	st.pulls["alice__repo"] = &model.PullState{
		ID: 1, PR: pr, SessionID: "thread-1", HeadSHA: "old999", BaseRef: "main", LastReviewID: 42,
	}
	prior := model.Finding{
		Path: "calc.go", Line: 19, Side: model.SideNew,
		Severity: model.SeverityHigh, Title: "div by zero", Body: "boom",
	}
	fp := prior.Fingerprint()
	st.storedFindings[1] = []model.StoredFinding{{
		Finding: prior, ID: 10, PullID: 1, Fingerprint: fp, FirstSeenSHA: "old999", Status: "open",
	}}
	cx := &fakeCodex{result: &model.ReviewResult{
		Summary: "rechecked", OverallSeverity: model.Severity("none"), SessionID: "thread-1",
	}}
	gt := &fakeGitea{diff: diffWith("calc.go", 19)}
	o := &Orchestrator{Store: st, Cache: &fakeCache{}, Reviewers: []model.Reviewer{cx}, Gitea: gt}

	if err := o.Process(context.Background(), prEvent("synchronized", "new222")); err != nil {
		t.Fatalf("Process: %v", err)
	}
	if !strings.Contains(cx.lastIn.Note, fp) {
		t.Fatalf("resume note does not include prior fingerprint %s: %q", fp, cx.lastIn.Note)
	}
	if gt.lastEvent != model.ReviewEventRequestChanges {
		t.Fatalf("event = %s, want REQUEST_CHANGES because prior high finding was carried", gt.lastEvent)
	}
	if len(gt.lastComments) != 1 || gt.lastComments[0].NewPosition != 19 {
		t.Fatalf("carried finding not posted inline: %+v", gt.lastComments)
	}
	if got := st.findings[1]; len(got) != 1 || got[0].Title != prior.Title {
		t.Fatalf("carried finding not persisted: %+v", got)
	}
}

func TestReview_Synchronized_MarksExplicitlyResolvedPriorFindingsFixed(t *testing.T) {
	st := newFakeStore()
	pr := model.PRRef{Owner: "alice", Repo: "repo", Number: 7}
	st.pulls["alice__repo"] = &model.PullState{
		ID: 1, PR: pr, SessionID: "thread-1", HeadSHA: "old999", BaseRef: "main", LastReviewID: 42,
	}
	prior := model.Finding{
		Path: "calc.go", Line: 19, Side: model.SideNew,
		Severity: model.SeverityHigh, Title: "div by zero", Body: "boom",
	}
	fp := prior.Fingerprint()
	st.storedFindings[1] = []model.StoredFinding{{
		Finding: prior, ID: 10, PullID: 1, Fingerprint: fp, FirstSeenSHA: "old999", Status: "open",
	}}
	cx := &fakeCodex{result: &model.ReviewResult{
		Summary: "fixed", OverallSeverity: model.Severity("none"), SessionID: "thread-1",
		ResolvedFingerprints: []string{fp},
	}}
	gt := &fakeGitea{diff: diffWith("calc.go", 19)}
	o := &Orchestrator{Store: st, Cache: &fakeCache{}, Reviewers: []model.Reviewer{cx}, Gitea: gt}

	if err := o.Process(context.Background(), prEvent("synchronized", "new222")); err != nil {
		t.Fatalf("Process: %v", err)
	}
	if gt.lastEvent != model.ReviewEventComment {
		t.Fatalf("event = %s, want COMMENT because prior finding was resolved", gt.lastEvent)
	}
	if len(gt.lastComments) != 0 {
		t.Fatalf("resolved finding should not be posted inline: %+v", gt.lastComments)
	}
	if got := st.fixed[1]; len(got) == 0 || !stringSet(got)[fp] {
		t.Fatalf("resolved fingerprint not marked fixed: %v", got)
	}
}

func TestReview_Synchronized_NormalizesUnprefixedResolvedFingerprint(t *testing.T) {
	st := newFakeStore()
	pr := model.PRRef{Owner: "alice", Repo: "repo", Number: 7}
	st.pulls["alice__repo"] = &model.PullState{
		ID: 1, PR: pr, SessionID: "thread-1", HeadSHA: "old999", BaseRef: "main", LastReviewID: 42,
	}
	prior := model.Finding{
		Path: "calc.go", Line: 19, Side: model.SideNew,
		Severity: model.SeverityHigh, Title: "div by zero", Body: "boom",
	}
	rawFP := prior.Fingerprint()
	prefixedFP := "codex:" + rawFP
	st.storedFindings[1] = []model.StoredFinding{{
		Finding: prior, ID: 10, PullID: 1, Fingerprint: prefixedFP, FirstSeenSHA: "old999", Status: "open",
	}}
	cx := &fakeCodex{result: &model.ReviewResult{
		Summary: "fixed", OverallSeverity: model.Severity("none"), SessionID: "thread-1",
		ResolvedFingerprints: []string{rawFP},
	}}
	gt := &fakeGitea{diff: diffWith("calc.go", 19)}
	o := &Orchestrator{Store: st, Cache: &fakeCache{}, Reviewers: []model.Reviewer{cx}, Gitea: gt}

	if err := o.Process(context.Background(), prEvent("synchronized", "new222")); err != nil {
		t.Fatalf("Process: %v", err)
	}
	if len(gt.lastComments) != 0 {
		t.Fatalf("resolved finding should not be carried forward inline: %+v", gt.lastComments)
	}
	if got := st.fixed[1]; len(got) == 0 || !stringSet(got)[prefixedFP] {
		t.Fatalf("resolved fingerprint not normalized to prefixed value: %v", got)
	}
}

func TestReview_CommentTrigger_ResumesAndAnswers(t *testing.T) {
	st := newFakeStore()
	st.pulls["alice__repo"] = &model.PullState{
		ID: 1, PR: model.PRRef{Owner: "alice", Repo: "repo", Number: 7},
		SessionID: "thread-1", HeadSHA: "h1", BaseRef: "main",
	}
	cx := &fakeCodex{askReply: "Yes, that fixes it."}
	gt := &fakeGitea{}
	o := &Orchestrator{
		Store: st, Cache: &fakeCache{}, Reviewers: []model.Reviewer{cx}, Gitea: gt,
		TriggerKeywords: []string{"/review"},
	}

	ev := model.WebhookEvent{
		Event: model.EventIssueComment, Action: "created", IsPR: true,
		PR:          model.PRRef{Owner: "alice", Repo: "repo", Number: 7},
		CommentBody: "/review is the divide-by-zero fixed now?",
		HeadRef:     "feat",
	}
	b, _ := json.Marshal(ev)
	if err := o.Process(context.Background(), &model.Job{Payload: b, PR: ev.PR, Event: ev.Event, Action: "created"}); err != nil {
		t.Fatalf("Process: %v", err)
	}
	if cx.lastAsk != "is the divide-by-zero fixed now?" {
		t.Errorf("question = %q", cx.lastAsk)
	}
	if len(gt.posted) != 1 || gt.posted[0] != "Yes, that fixes it." {
		t.Errorf("answer not posted: %v", gt.posted)
	}
}

func TestReview_CommentTrigger_NoSession(t *testing.T) {
	st := newFakeStore() // no prior pull
	gt := &fakeGitea{}
	o := &Orchestrator{
		Store: st, Cache: &fakeCache{}, Reviewers: []model.Reviewer{&fakeCodex{}}, Gitea: gt,
		TriggerKeywords: []string{"/review"},
	}
	ev := model.WebhookEvent{
		Event: model.EventIssueComment, Action: "created", IsPR: true,
		PR:          model.PRRef{Owner: "alice", Repo: "repo", Number: 7},
		CommentBody: "/review please",
	}
	b, _ := json.Marshal(ev)
	if err := o.Process(context.Background(), &model.Job{Payload: b, PR: ev.PR, Event: ev.Event, Action: "created"}); err != nil {
		t.Fatalf("Process: %v", err)
	}
	if len(gt.posted) != 1 {
		t.Fatalf("want 1 explanatory comment, got %d", len(gt.posted))
	}
}

func TestReview_RepoAllowlist(t *testing.T) {
	gt := &fakeGitea{diff: diffWith("x", 1)}
	o := &Orchestrator{
		Store: newFakeStore(), Cache: &fakeCache{},
		Reviewers: []model.Reviewer{&fakeCodex{result: &model.ReviewResult{}}}, Gitea: gt,
		RepoAllowlist: []string{"someone/else"},
	}
	if err := o.Process(context.Background(), prEvent("opened", "h")); err != nil {
		t.Fatalf("Process: %v", err)
	}
	if gt.reviewID != 0 {
		t.Error("review posted for repo not in allowlist")
	}
}
