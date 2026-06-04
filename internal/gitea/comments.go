package gitea

import (
	"context"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/turning4th/codex-gitea/internal/model"
)

// commentRequest is the POST body for an issue comment.
type commentRequest struct {
	Body string `json:"body"`
}

func parseTime(s string) time.Time {
	t, err := time.Parse(time.RFC3339, s)
	if err != nil {
		return time.Time{}
	}
	return t
}

// commentResponse captures the id we read back.
type commentResponse struct {
	ID        int64     `json:"id"`
	Body      string    `json:"body"`
	User      *userInfo `json:"user"`
	CreatedAt string    `json:"created_at"`
}

// PostComment posts a plain (non-review) comment on the PR's issue thread.
func (c *Client) PostComment(ctx context.Context, pr model.PRRef, body string) (int64, error) {
	path := c.repoPath(pr, "/issues/"+strconv.Itoa(pr.Number)+"/comments")
	var resp commentResponse
	if err := c.doJSON(ctx, "POST", path, commentRequest{Body: body}, &resp); err != nil {
		return 0, err
	}
	return resp.ID, nil
}

// ListIssueComments returns regular issue-thread comments for a PR. Gitea
// models pull requests as issues, so this is where bot mentions and reviewer
// requests like "@bot check agents.md" live.
func (c *Client) ListIssueComments(ctx context.Context, pr model.PRRef) ([]model.PullComment, error) {
	q := url.Values{}
	q.Set("limit", "20")
	path := c.repoPath(pr, "/issues/"+strconv.Itoa(pr.Number)+"/comments") + "?" + q.Encode()
	var resp []commentResponse
	if err := c.doJSON(ctx, "GET", path, nil, &resp); err != nil {
		return nil, err
	}
	out := make([]model.PullComment, 0, len(resp))
	for _, r := range resp {
		user := ""
		if r.User != nil {
			user = r.User.Username
			if user == "" {
				user = r.User.Login
			}
		}
		out = append(out, model.PullComment{
			ID:        r.ID,
			User:      user,
			Body:      strings.TrimSpace(r.Body),
			CreatedAt: parseTime(r.CreatedAt),
		})
	}
	return out, nil
}
