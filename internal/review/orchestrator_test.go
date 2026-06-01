package review

import (
	"context"
	"encoding/json"
	"sync"
	"testing"

	"github.com/turning4th/codex-gitea/internal/model"
)

// ---------- fakes ----------

type fakeStore struct {
	mu       sync.Mutex
	pulls    map[string]*model.PullState
	findings map[int64][]model.Finding
	lastRev  map[string]int64
}

func newFakeStore() *fakeStore {
	return &fakeStore{
		pulls:    map[string]*model.PullState{},
		findings: map[int64][]model.Finding{},
		lastRev:  map[string]int64{},
	}
}

func pk(pr model.PRRef) string { return pr.Key() }

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

func (s *fakeStore) SaveFindings(_ context.Context, pullID int64, _ string, fs []model.Finding) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.findings[pullID] = fs
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
func (s *fakeStore) ListJobs(context.Context, int) ([]model.JobView, error)          { return nil, nil }
func (s *fakeStore) GetJob(context.Context, int64) (*model.Job, error)               { return nil, nil }
func (s *fakeStore) ListFindings(context.Context, int64) ([]model.StoredFinding, error) {
	return nil, nil
}
func (s *fakeStore) MarkFindingPosted(context.Context, int64, int64, int64) error { return nil }
func (s *fakeStore) GetSetting(context.Context, string) (string, bool, error)     { return "", false, nil }
func (s *fakeStore) SetSetting(context.Context, string, string, bool) error       { return nil }
func (s *fakeStore) AllSettings(context.Context) (map[string]string, error)       { return nil, nil }
func (s *fakeStore) Close() error                                                 { return nil }

type fakeCache struct{ prepared, cleaned int }

func (c *fakeCache) Prepare(_ context.Context, _ model.PRRef, _, _, _, _ string) (string, error) {
	c.prepared++
	return "/work/fake", nil
}
func (c *fakeCache) Cleanup(model.PRRef) error { c.cleaned++; return nil }

type fakeCodex struct {
	result   *model.ReviewResult
	lastIn   model.CodexInput
	askReply string
	lastAsk  string
}

func (c *fakeCodex) Review(_ context.Context, in model.CodexInput) (*model.ReviewResult, error) {
	c.lastIn = in
	return c.result, nil
}
func (c *fakeCodex) Ask(_ context.Context, _, _, q string) (string, error) {
	c.lastAsk = q
	return c.askReply, nil
}
func (c *fakeCodex) Status(context.Context) (string, error) { return "ok", nil }

type fakeGitea struct {
	diff         model.DiffMap
	lastEvent    model.ReviewEventType
	lastComments []model.ReviewComment
	dismissed    []int64
	posted       []string
	reviewID     int64
}

func (g *fakeGitea) GetDiff(context.Context, model.PRRef) (model.DiffMap, error) {
	return g.diff, nil
}
func (g *fakeGitea) PostReview(_ context.Context, _ model.PRRef, _ string, ev model.ReviewEventType, _ string, cs []model.ReviewComment) (int64, error) {
	g.lastEvent = ev
	g.lastComments = cs
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

func diffWith(path string, lines ...int) model.DiffMap {
	nl := map[int]bool{}
	for _, l := range lines {
		nl[l] = true
	}
	return model.DiffMap{Files: map[string]model.FileDiff{
		path: {NewLines: nl, OldLines: map[int]bool{}},
	}}
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
	o := &Orchestrator{Store: st, Cache: gc, Codex: cx, Gitea: gt}

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
	o := &Orchestrator{Store: st, Cache: gc, Codex: cx, Gitea: gt}

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

func TestReview_CommentTrigger_ResumesAndAnswers(t *testing.T) {
	st := newFakeStore()
	st.pulls["alice__repo"] = &model.PullState{
		ID: 1, PR: model.PRRef{Owner: "alice", Repo: "repo", Number: 7},
		SessionID: "thread-1", HeadSHA: "h1", BaseRef: "main",
	}
	cx := &fakeCodex{askReply: "Yes, that fixes it."}
	gt := &fakeGitea{}
	o := &Orchestrator{
		Store: st, Cache: &fakeCache{}, Codex: cx, Gitea: gt,
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
		Store: st, Cache: &fakeCache{}, Codex: &fakeCodex{}, Gitea: gt,
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
		Codex: &fakeCodex{result: &model.ReviewResult{}}, Gitea: gt,
		RepoAllowlist: []string{"someone/else"},
	}
	if err := o.Process(context.Background(), prEvent("opened", "h")); err != nil {
		t.Fatalf("Process: %v", err)
	}
	if gt.reviewID != 0 {
		t.Error("review posted for repo not in allowlist")
	}
}
