package store

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/turning4th/codex-gitea/internal/model"
)

func newTestStore(t *testing.T) *Store {
	t.Helper()
	dbPath := filepath.Join(t.TempDir(), "test.db")
	st, err := Open(dbPath)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	return st
}

func sampleEvent(deliveryID string) *model.WebhookEvent {
	return &model.WebhookEvent{
		DeliveryID: deliveryID,
		Event:      model.EventPullRequest,
		Action:     "opened",
		PR:         model.PRRef{Owner: "acme", Repo: "widgets", Number: 7},
		BaseRef:    "main",
		HeadRef:    "feature",
		HeadSHA:    "abc123",
		CloneURL:   "https://git.example/acme/widgets.git",
		Raw:        []byte(`{"raw":true}`),
	}
}

func TestEnqueueJobDedup(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()

	job, created, err := st.EnqueueJob(ctx, sampleEvent("delivery-1"))
	if err != nil {
		t.Fatalf("EnqueueJob: %v", err)
	}
	if !created {
		t.Fatalf("first enqueue: created=false, want true")
	}
	if job.ID == 0 {
		t.Fatalf("first enqueue: got zero job id")
	}
	if job.Status != model.JobPending {
		t.Fatalf("first enqueue: status=%q, want pending", job.Status)
	}
	if job.PR.Owner != "acme" || job.PR.Repo != "widgets" || job.PR.Number != 7 {
		t.Fatalf("first enqueue: PR roundtrip mismatch: %+v", job.PR)
	}
	if len(job.Payload) == 0 {
		t.Fatalf("first enqueue: payload not persisted")
	}

	// Same delivery id => dedup, created=false, no error, same job id.
	dup, created, err := st.EnqueueJob(ctx, sampleEvent("delivery-1"))
	if err != nil {
		t.Fatalf("EnqueueJob dup: %v", err)
	}
	if created {
		t.Fatalf("dup enqueue: created=true, want false")
	}
	if dup.ID != job.ID {
		t.Fatalf("dup enqueue: id=%d, want %d", dup.ID, job.ID)
	}
}

func TestClaimAndFinishJob(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()

	if _, _, err := st.EnqueueJob(ctx, sampleEvent("d-a")); err != nil {
		t.Fatalf("enqueue a: %v", err)
	}
	ev2 := sampleEvent("d-b")
	ev2.PR.Number = 8
	if _, _, err := st.EnqueueJob(ctx, ev2); err != nil {
		t.Fatalf("enqueue b: %v", err)
	}

	// Claims oldest first (PR 7).
	claimed, err := st.ClaimJob(ctx)
	if err != nil {
		t.Fatalf("ClaimJob: %v", err)
	}
	if claimed == nil {
		t.Fatalf("ClaimJob: got nil, want a job")
	}
	if claimed.PR.Number != 7 {
		t.Fatalf("ClaimJob: claimed PR %d, want oldest (7)", claimed.PR.Number)
	}
	if claimed.Status != model.JobRunning {
		t.Fatalf("ClaimJob: status=%q, want running", claimed.Status)
	}
	if claimed.Attempts != 1 {
		t.Fatalf("ClaimJob: attempts=%d, want 1", claimed.Attempts)
	}
	if claimed.StartedAt == nil {
		t.Fatalf("ClaimJob: started_at not set")
	}

	// Finish it.
	if err := st.FinishJob(ctx, claimed.ID, model.JobDone, ""); err != nil {
		t.Fatalf("FinishJob: %v", err)
	}
	got, err := st.GetJob(ctx, claimed.ID)
	if err != nil {
		t.Fatalf("GetJob: %v", err)
	}
	if got.Status != model.JobDone {
		t.Fatalf("after finish: status=%q, want done", got.Status)
	}
	if got.FinishedAt == nil {
		t.Fatalf("after finish: finished_at not set")
	}

	// Next claim returns the second job (PR 8).
	claimed2, err := st.ClaimJob(ctx)
	if err != nil {
		t.Fatalf("ClaimJob 2: %v", err)
	}
	if claimed2 == nil || claimed2.PR.Number != 8 {
		t.Fatalf("ClaimJob 2: got %+v, want PR 8", claimed2)
	}

	// No more pending => nil, nil.
	none, err := st.ClaimJob(ctx)
	if err != nil {
		t.Fatalf("ClaimJob empty: %v", err)
	}
	if none != nil {
		t.Fatalf("ClaimJob empty: got %+v, want nil", none)
	}
}

func TestRecoverRunning(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()

	if _, _, err := st.EnqueueJob(ctx, sampleEvent("d-recover")); err != nil {
		t.Fatalf("enqueue: %v", err)
	}
	claimed, err := st.ClaimJob(ctx)
	if err != nil || claimed == nil {
		t.Fatalf("ClaimJob: %v / %v", claimed, err)
	}
	if claimed.Status != model.JobRunning {
		t.Fatalf("precondition: status=%q, want running", claimed.Status)
	}

	if err := st.RecoverRunning(ctx); err != nil {
		t.Fatalf("RecoverRunning: %v", err)
	}
	got, err := st.GetJob(ctx, claimed.ID)
	if err != nil {
		t.Fatalf("GetJob: %v", err)
	}
	if got.Status != model.JobPending {
		t.Fatalf("after recover: status=%q, want pending", got.Status)
	}
	if got.StartedAt != nil {
		t.Fatalf("after recover: started_at should be cleared")
	}

	// Recovered job is claimable again.
	reclaimed, err := st.ClaimJob(ctx)
	if err != nil || reclaimed == nil {
		t.Fatalf("reclaim: %v / %v", reclaimed, err)
	}
	if reclaimed.ID != claimed.ID {
		t.Fatalf("reclaim: id=%d, want %d", reclaimed.ID, claimed.ID)
	}
}

func TestSupersedePending(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()

	pr := model.PRRef{Owner: "acme", Repo: "widgets", Number: 7}
	if _, _, err := st.EnqueueJob(ctx, sampleEvent("d-super")); err != nil {
		t.Fatalf("enqueue: %v", err)
	}
	if err := st.SupersedePending(ctx, pr); err != nil {
		t.Fatalf("SupersedePending: %v", err)
	}
	// No pending jobs should remain.
	none, err := st.ClaimJob(ctx)
	if err != nil {
		t.Fatalf("ClaimJob: %v", err)
	}
	if none != nil {
		t.Fatalf("after supersede: claimed %+v, want nil", none)
	}

	// Superseding an unknown repo is a no-op, not an error.
	if err := st.SupersedePending(ctx, model.PRRef{Owner: "ghost", Repo: "none", Number: 1}); err != nil {
		t.Fatalf("SupersedePending unknown: %v", err)
	}
}

func TestListJobs(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()

	if _, _, err := st.EnqueueJob(ctx, sampleEvent("d-list-1")); err != nil {
		t.Fatalf("enqueue 1: %v", err)
	}
	ev := sampleEvent("d-list-2")
	ev.PR.Number = 9
	if _, _, err := st.EnqueueJob(ctx, ev); err != nil {
		t.Fatalf("enqueue 2: %v", err)
	}

	views, err := st.ListJobs(ctx, 10)
	if err != nil {
		t.Fatalf("ListJobs: %v", err)
	}
	if len(views) != 2 {
		t.Fatalf("ListJobs: got %d, want 2", len(views))
	}
	// Newest first.
	if views[0].PR.Number != 9 {
		t.Fatalf("ListJobs order: first PR %d, want 9", views[0].PR.Number)
	}
	if views[0].PR.Owner != "acme" || views[0].PR.Repo != "widgets" {
		t.Fatalf("ListJobs: repo join mismatch: %+v", views[0].PR)
	}
}

func TestUpsertAndGetPull(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()

	pr := model.PRRef{Owner: "acme", Repo: "widgets", Number: 42}

	// Missing pull => nil, nil.
	missing, err := st.GetPull(ctx, pr)
	if err != nil {
		t.Fatalf("GetPull missing: %v", err)
	}
	if missing != nil {
		t.Fatalf("GetPull missing: got %+v, want nil", missing)
	}

	in := &model.PullState{
		PR:           pr,
		SessionID:    "sess-1",
		HeadSHA:      "deadbeef",
		BaseRef:      "main",
		LastReviewID: 100,
	}
	if err := st.UpsertPull(ctx, in); err != nil {
		t.Fatalf("UpsertPull: %v", err)
	}

	got, err := st.GetPull(ctx, pr)
	if err != nil {
		t.Fatalf("GetPull: %v", err)
	}
	if got == nil {
		t.Fatalf("GetPull: got nil after upsert")
	}
	if got.SessionID != "sess-1" || got.HeadSHA != "deadbeef" ||
		got.BaseRef != "main" || got.LastReviewID != 100 {
		t.Fatalf("GetPull roundtrip mismatch: %+v", got)
	}
	if got.PR != pr {
		t.Fatalf("GetPull PR mismatch: %+v", got.PR)
	}
	if got.ID == 0 {
		t.Fatalf("GetPull: id not populated")
	}

	// Update overwrites fields.
	in.SessionID = "sess-2"
	in.LastReviewID = 200
	if err := st.UpsertPull(ctx, in); err != nil {
		t.Fatalf("UpsertPull update: %v", err)
	}
	got2, err := st.GetPull(ctx, pr)
	if err != nil {
		t.Fatalf("GetPull 2: %v", err)
	}
	if got2.SessionID != "sess-2" || got2.LastReviewID != 200 {
		t.Fatalf("GetPull after update mismatch: %+v", got2)
	}
	if got2.ID != got.ID {
		t.Fatalf("GetPull after update: id changed %d -> %d", got.ID, got2.ID)
	}
}

func TestSetSessionAndLastReview(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()

	pr := model.PRRef{Owner: "acme", Repo: "gizmos", Number: 3}

	// SetSession on a fresh PR auto-creates the pull row.
	if err := st.SetSession(ctx, pr, "thread-xyz"); err != nil {
		t.Fatalf("SetSession: %v", err)
	}
	got, err := st.GetPull(ctx, pr)
	if err != nil {
		t.Fatalf("GetPull: %v", err)
	}
	if got == nil || got.SessionID != "thread-xyz" {
		t.Fatalf("SetSession roundtrip: %+v", got)
	}

	if err := st.SetLastReview(ctx, pr, 555); err != nil {
		t.Fatalf("SetLastReview: %v", err)
	}
	got2, err := st.GetPull(ctx, pr)
	if err != nil {
		t.Fatalf("GetPull 2: %v", err)
	}
	if got2.LastReviewID != 555 {
		t.Fatalf("SetLastReview: last_review_id=%d, want 555", got2.LastReviewID)
	}
	// Session should survive setting the review.
	if got2.SessionID != "thread-xyz" {
		t.Fatalf("SetLastReview clobbered session: %q", got2.SessionID)
	}
}

func TestSaveAndListFindings(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()

	pr := model.PRRef{Owner: "acme", Repo: "widgets", Number: 11}
	if err := st.UpsertPull(ctx, &model.PullState{PR: pr, HeadSHA: "sha0"}); err != nil {
		t.Fatalf("UpsertPull: %v", err)
	}
	pull, err := st.GetPull(ctx, pr)
	if err != nil {
		t.Fatalf("GetPull: %v", err)
	}

	fs := []model.Finding{
		{Path: "a.go", Line: 10, Side: model.SideNew, Severity: model.SeverityHigh, Title: "bug A", Body: "x"},
		{Path: "b.go", Line: 20, Side: model.SideNew, Severity: model.SeverityLow, Title: "nit B", Body: "y"},
	}
	if err := st.SaveFindings(ctx, pull.ID, "sha1", fs); err != nil {
		t.Fatalf("SaveFindings: %v", err)
	}

	got, err := st.ListFindings(ctx, pull.ID)
	if err != nil {
		t.Fatalf("ListFindings: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("ListFindings: got %d, want 2", len(got))
	}
	for _, sf := range got {
		if sf.Status != "open" {
			t.Fatalf("finding %q status=%q, want open", sf.Path, sf.Status)
		}
		if sf.FirstSeenSHA != "sha1" {
			t.Fatalf("finding %q first_seen_sha=%q, want sha1", sf.Path, sf.FirstSeenSHA)
		}
		if sf.Fingerprint == "" {
			t.Fatalf("finding %q has empty fingerprint", sf.Path)
		}
	}

	// Re-save the same findings with a new head SHA: upsert by fingerprint,
	// no duplicate rows, first_seen_sha preserved.
	if err := st.SaveFindings(ctx, pull.ID, "sha2", fs); err != nil {
		t.Fatalf("SaveFindings resave: %v", err)
	}
	got2, err := st.ListFindings(ctx, pull.ID)
	if err != nil {
		t.Fatalf("ListFindings 2: %v", err)
	}
	if len(got2) != 2 {
		t.Fatalf("ListFindings after resave: got %d, want 2 (dedup by fingerprint)", len(got2))
	}
	if got2[0].FirstSeenSHA != "sha1" {
		t.Fatalf("resave clobbered first_seen_sha: %q", got2[0].FirstSeenSHA)
	}

	// MarkFindingPosted records comment + review ids.
	if err := st.MarkFindingPosted(ctx, got2[0].ID, 999, 888); err != nil {
		t.Fatalf("MarkFindingPosted: %v", err)
	}
	got3, err := st.ListFindings(ctx, pull.ID)
	if err != nil {
		t.Fatalf("ListFindings 3: %v", err)
	}
	if got3[0].GiteaCommentID != 999 || got3[0].ReviewID != 888 {
		t.Fatalf("MarkFindingPosted roundtrip: comment=%d review=%d",
			got3[0].GiteaCommentID, got3[0].ReviewID)
	}
}

func TestSettingsRoundtrip(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()

	// Missing key => found=false.
	if _, found, err := st.GetSetting(ctx, "nope"); err != nil || found {
		t.Fatalf("GetSetting missing: found=%v err=%v", found, err)
	}

	if err := st.SetSetting(ctx, "model", "gpt-5", false); err != nil {
		t.Fatalf("SetSetting: %v", err)
	}
	if err := st.SetSetting(ctx, "token", "secret-val", true); err != nil {
		t.Fatalf("SetSetting secret: %v", err)
	}

	v, found, err := st.GetSetting(ctx, "model")
	if err != nil || !found {
		t.Fatalf("GetSetting model: found=%v err=%v", found, err)
	}
	if v != "gpt-5" {
		t.Fatalf("GetSetting model: %q, want gpt-5", v)
	}

	// Update existing key.
	if err := st.SetSetting(ctx, "model", "gpt-6", false); err != nil {
		t.Fatalf("SetSetting update: %v", err)
	}
	v, _, _ = st.GetSetting(ctx, "model")
	if v != "gpt-6" {
		t.Fatalf("GetSetting after update: %q, want gpt-6", v)
	}

	all, err := st.AllSettings(ctx)
	if err != nil {
		t.Fatalf("AllSettings: %v", err)
	}
	if all["model"] != "gpt-6" || all["token"] != "secret-val" {
		t.Fatalf("AllSettings mismatch: %+v", all)
	}
}
