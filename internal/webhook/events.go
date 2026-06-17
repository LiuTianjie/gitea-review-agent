package webhook

import (
	"encoding/json"
	"fmt"

	"github.com/turning4th/codex-gitea/internal/model"
)

// ---------- Gitea payload structs (only the fields we consume) ----------

type giteaUser struct {
	Username string `json:"username"`
	Login    string `json:"login"`
}

func (u giteaUser) name() string {
	if u.Username != "" {
		return u.Username
	}
	return u.Login
}

type giteaRepository struct {
	Name     string    `json:"name"`
	Owner    giteaUser `json:"owner"`
	CloneURL string    `json:"clone_url"`
}

type giteaBranch struct {
	Ref string `json:"ref"`
	SHA string `json:"sha"`
}

type giteaPullRequest struct {
	Base   giteaBranch `json:"base"`
	Head   giteaBranch `json:"head"`
	User   giteaUser   `json:"user"`
	Poster giteaUser   `json:"poster"`
}

func (pr giteaPullRequest) author() string {
	if name := pr.User.name(); name != "" {
		return name
	}
	return pr.Poster.name()
}

// pullRequestPayload models the body sent for the X-Gitea-Event: pull_request.
type pullRequestPayload struct {
	Action      string           `json:"action"`
	Number      int              `json:"number"`
	PullRequest giteaPullRequest `json:"pull_request"`
	Repository  giteaRepository  `json:"repository"`
}

type giteaComment struct {
	Body string    `json:"body"`
	User giteaUser `json:"user"`
}

type giteaIssue struct {
	Number int       `json:"number"`
	User   giteaUser `json:"user"`
	// PullRequest is present and non-null when the issue is a pull request.
	// json.RawMessage lets us distinguish "absent/null" from "{}".
	PullRequest json.RawMessage `json:"pull_request"`
}

// issueCommentPayload models the body sent for X-Gitea-Event: issue_comment.
type issueCommentPayload struct {
	Action     string          `json:"action"`
	Issue      giteaIssue      `json:"issue"`
	Comment    giteaComment    `json:"comment"`
	Repository giteaRepository `json:"repository"`
}

// isPRPayload reports whether issue.pull_request is present and non-null.
func isPRPayload(raw json.RawMessage) bool {
	if len(raw) == 0 {
		return false
	}
	// json.RawMessage of a JSON null is the 4 bytes "null".
	if string(raw) == "null" {
		return false
	}
	return true
}

// Parse turns a raw, already-verified Gitea webhook body into a normalized
// model.WebhookEvent based on eventType (the X-Gitea-Event header value).
// An unrecognized eventType returns an error.
func Parse(eventType string, body []byte) (*model.WebhookEvent, error) {
	switch model.EventType(eventType) {
	case model.EventPullRequest, model.EventPullRequestSync:
		var p pullRequestPayload
		if err := json.Unmarshal(body, &p); err != nil {
			return nil, fmt.Errorf("webhook: parse pull_request payload: %w", err)
		}
		return &model.WebhookEvent{
			Event:  model.EventPullRequest,
			Action: p.Action,
			PR: model.PRRef{
				Owner:  p.Repository.Owner.name(),
				Repo:   p.Repository.Name,
				Number: p.Number,
			},
			Author:   p.PullRequest.author(),
			BaseRef:  p.PullRequest.Base.Ref,
			HeadRef:  p.PullRequest.Head.Ref,
			HeadSHA:  p.PullRequest.Head.SHA,
			CloneURL: p.Repository.CloneURL,
			Raw:      body,
		}, nil

	case model.EventIssueComment:
		var p issueCommentPayload
		if err := json.Unmarshal(body, &p); err != nil {
			return nil, fmt.Errorf("webhook: parse issue_comment payload: %w", err)
		}
		return &model.WebhookEvent{
			Event:  model.EventIssueComment,
			Action: p.Action,
			PR: model.PRRef{
				Owner:  p.Repository.Owner.name(),
				Repo:   p.Repository.Name,
				Number: p.Issue.Number,
			},
			Author:      p.Issue.User.name(),
			CloneURL:    p.Repository.CloneURL,
			IsPR:        isPRPayload(p.Issue.PullRequest),
			CommentBody: p.Comment.Body,
			CommentUser: p.Comment.User.Username,
			Raw:         body,
		}, nil

	default:
		return nil, fmt.Errorf("webhook: unsupported event type %q", eventType)
	}
}
