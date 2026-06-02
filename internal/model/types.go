// Package model defines shared domain types and module interfaces (ports).
// Concrete implementations live in sibling packages and depend only on this.
package model

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"time"
)

// ---------- Identifiers ----------

// PRRef identifies a pull request within a Gitea instance.
type PRRef struct {
	Owner  string
	Repo   string
	Number int
}

// Key returns a stable filesystem-safe key for caches and locks.
func (p PRRef) Key() string { return p.Owner + "__" + p.Repo }

// ---------- Enums ----------

// Severity ranks the importance of a finding.
type Severity string

const (
	SeverityInfo     Severity = "info"
	SeverityLow      Severity = "low"
	SeverityMedium   Severity = "medium"
	SeverityHigh     Severity = "high"
	SeverityCritical Severity = "critical"
)

// Blocking reports whether a severity should trigger REQUEST_CHANGES.
func (s Severity) Blocking() bool { return s == SeverityHigh || s == SeverityCritical }

// Side indicates whether a finding/line refers to the new or old file.
type Side string

const (
	SideNew Side = "NEW"
	SideOld Side = "OLD"
)

// EventType is the Gitea webhook event class (X-Gitea-Event header).
type EventType string

const (
	EventPullRequest     EventType = "pull_request"
	EventPullRequestSync EventType = "pull_request_sync"
	EventIssueComment    EventType = "issue_comment"
)

// JobStatus tracks a job through the queue.
type JobStatus string

const (
	JobPending    JobStatus = "pending"
	JobRunning    JobStatus = "running"
	JobDone       JobStatus = "done"
	JobFailed     JobStatus = "failed"
	JobSuperseded JobStatus = "superseded"
)

// ReviewEventType is the Gitea review verdict.
type ReviewEventType string

const (
	ReviewEventComment        ReviewEventType = "COMMENT"
	ReviewEventRequestChanges ReviewEventType = "REQUEST_CHANGES"
	ReviewEventApproved       ReviewEventType = "APPROVED"
)

// ---------- Webhook / Job ----------

// WebhookEvent is the normalized result of parsing a Gitea webhook payload.
type WebhookEvent struct {
	DeliveryID string
	Event      EventType
	Action     string // opened|synchronized|edited|closed|reopened (PR); created|edited|deleted (comment)
	PR         PRRef
	BaseRef    string
	HeadRef    string
	HeadSHA    string
	CloneURL   string

	// issue_comment only
	IsPR        bool // issue.pull_request != null
	CommentBody string
	CommentUser string

	Raw []byte // raw verified body, persisted for replay/debug
}

// Job is a queued unit of work derived from a WebhookEvent.
type Job struct {
	ID         int64
	DeliveryID string
	PR         PRRef
	Event      EventType
	Action     string
	Payload    []byte // serialized WebhookEvent
	Status     JobStatus
	Attempts   int
	Error      string
	CreatedAt  time.Time
	StartedAt  *time.Time
	FinishedAt *time.Time
}

// ---------- Codex / Review ----------

// Finding is one issue reported by codex, before diff-position mapping.
type Finding struct {
	Path     string   `json:"path"`
	Line     int      `json:"line"`
	Side     Side     `json:"side"`
	Severity Severity `json:"severity"`
	Title    string   `json:"title"`
	Body     string   `json:"body"`
}

// Fingerprint is a stable hash key for dedup across re-reviews.
// Computed by the store layer; defined here so producers agree on inputs.
func (f Finding) FingerprintInput() string {
	return f.Path + "|" + f.Title + "|" + string(f.Severity)
}

// Fingerprint is a stable sha256 key for deduplicating findings across runs.
func (f Finding) Fingerprint() string {
	sum := sha256.Sum256([]byte(f.FingerprintInput()))
	return hex.EncodeToString(sum[:])
}

// ReviewResult is the structured output schema codex returns.
type ReviewResult struct {
	Summary              string    `json:"summary"`
	OverallSeverity      Severity  `json:"overall_severity"`
	Findings             []Finding `json:"findings"`
	ResolvedFingerprints []string  `json:"resolved_fingerprints"`

	// SessionID is the codex thread_id (from thread.started), filled by the runner.
	SessionID string `json:"-"`
}

// PullState is the persisted per-PR state (session continuity, last review).
type PullState struct {
	ID           int64
	PR           PRRef
	SessionID    string
	HeadSHA      string
	BaseRef      string
	LastReviewID int64
	UpdatedAt    time.Time
}

// StoredFinding is a Finding with persistence metadata.
type StoredFinding struct {
	Finding
	ID             int64
	PullID         int64
	Fingerprint    string
	GiteaCommentID int64
	ReviewID       int64
	FirstSeenSHA   string
	Status         string // open|fixed
}

// ---------- Diff mapping ----------

// FileDiff records which line numbers in a file fall inside the PR diff hunks.
type FileDiff struct {
	NewLines map[int]bool
	OldLines map[int]bool
}

// DiffMap maps changed lines per file; built by the gitea diff parser and
// consumed by the orchestrator to decide which findings can be inline comments.
type DiffMap struct {
	Files map[string]FileDiff
}

// Position resolves a finding's (path,line,side) to Gitea review-comment
// positions. ok=false means the line is outside the diff (fold into summary).
func (d DiffMap) Position(path string, line int, side Side) (newPos, oldPos int64, ok bool) {
	fd, exists := d.Files[path]
	if !exists {
		return 0, 0, false
	}
	if side == SideOld {
		if fd.OldLines[line] {
			return 0, int64(line), true
		}
		return 0, 0, false
	}
	if fd.NewLines[line] {
		return int64(line), 0, true
	}
	return 0, 0, false
}

// ReviewComment is one inline comment in a Gitea review submission.
type ReviewComment struct {
	Path        string `json:"path"`
	Body        string `json:"body"`
	NewPosition int64  `json:"new_position"`
	OldPosition int64  `json:"old_position"`
}

// GiteaReview is a summary of an existing review (for listing/dismissing).
type GiteaReview struct {
	ID    int64
	State string // APPROVED|REQUEST_CHANGES|COMMENT|PENDING|DISMISSED
	Body  string
	Stale bool
}

// ---------- Console views ----------

// JobView is a read model for the console job list.
type JobView struct {
	ID         int64
	PR         PRRef
	Event      EventType
	Action     string
	Status     JobStatus
	Attempts   int
	Error      string
	CreatedAt  time.Time
	StartedAt  *time.Time
	FinishedAt *time.Time
	SessionID  string
	Logs       []JobLog
}

// JobLog is a timestamped progress entry for a queued job.
type JobLog struct {
	ID        int64
	Stage     string
	Message   string
	CreatedAt time.Time
}

// JobStats summarizes queue/review health for the console dashboard.
type JobStats struct {
	TotalJobs    int
	ReviewJobs   int
	ReviewedJobs int
	Done         int
	Failed       int
	Running      int
	Pending      int
	Superseded   int
}

// ---------- Ports (interfaces) ----------

// Store persists jobs, PR state, findings, and console-editable settings.
type Store interface {
	// Jobs
	EnqueueJob(ctx context.Context, ev *WebhookEvent) (job *Job, created bool, err error)
	SupersedePending(ctx context.Context, pr PRRef) error
	ClaimJob(ctx context.Context) (*Job, error) // next pending -> running; nil if none
	FinishJob(ctx context.Context, id int64, status JobStatus, errMsg string) error
	RecoverRunning(ctx context.Context) error // running -> pending on boot
	AppendJobLog(ctx context.Context, jobID int64, stage, message string) error
	ListJobs(ctx context.Context, limit, offset int) ([]JobView, error)
	CountJobs(ctx context.Context) (int, error)
	JobStats(ctx context.Context) (JobStats, error)
	GetJob(ctx context.Context, id int64) (*Job, error)

	// Pulls / sessions
	GetPull(ctx context.Context, pr PRRef) (*PullState, error)
	UpsertPull(ctx context.Context, st *PullState) error
	SetSession(ctx context.Context, pr PRRef, sessionID string) error
	SetLastReview(ctx context.Context, pr PRRef, reviewID int64) error

	// Findings
	SaveFindings(ctx context.Context, pullID int64, headSHA string, fs []Finding) error
	MarkFindingsFixed(ctx context.Context, pullID int64, fingerprints []string) error
	ListFindings(ctx context.Context, pullID int64) ([]StoredFinding, error)
	MarkFindingPosted(ctx context.Context, findingID, commentID, reviewID int64) error

	// Settings (console-editable runtime config)
	GetSetting(ctx context.Context, key string) (string, bool, error)
	SetSetting(ctx context.Context, key, value string, isSecret bool) error
	AllSettings(ctx context.Context) (map[string]string, error)

	Close() error
}

// GitCache manages per-repo bare mirrors and throwaway worktrees.
type GitCache interface {
	// Prepare ensures the mirror exists, fetches base+PR refs, and checks out
	// a deterministic worktree at headSHA. Returns the worktree path.
	Prepare(ctx context.Context, pr PRRef, cloneURL, baseRef, headRef, headSHA string) (worktree string, err error)
	// Cleanup removes the worktree but keeps the mirror.
	Cleanup(pr PRRef) error
}

// CodexInput parameterizes a codex review run.
type CodexInput struct {
	Worktree  string
	BaseRef   string
	SessionID string // empty = new review; set = resume an existing thread
	Note      string // optional extra context (e.g. "head moved A->B, note what's fixed")
}

// CodexRunner invokes the codex CLI in headless mode.
type CodexRunner interface {
	// Review runs a structured review (new or resume) and returns findings + session id.
	Review(ctx context.Context, in CodexInput) (*ReviewResult, error)
	// Ask resumes a session with a free-form question and returns the text answer.
	Ask(ctx context.Context, sessionID, worktree, question string) (string, error)
	// Status reports whether codex auth is currently valid (codex login status).
	Status(ctx context.Context) (string, error)
}

// GiteaClient talks to the Gitea REST API.
type GiteaClient interface {
	GetDiff(ctx context.Context, pr PRRef) (DiffMap, error)
	PostReview(ctx context.Context, pr PRRef, commitID string, event ReviewEventType, body string, comments []ReviewComment) (reviewID int64, err error)
	PostComment(ctx context.Context, pr PRRef, body string) (commentID int64, err error)
	ListReviews(ctx context.Context, pr PRRef) ([]GiteaReview, error)
	DismissReview(ctx context.Context, pr PRRef, reviewID int64, msg string) error
}
