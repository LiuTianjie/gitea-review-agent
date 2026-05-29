package gitea

import (
	"context"
	"strconv"

	"github.com/turning4th/codex-gitea/internal/model"
)

// commentRequest is the POST body for an issue comment.
type commentRequest struct {
	Body string `json:"body"`
}

// commentResponse captures the id we read back.
type commentResponse struct {
	ID int64 `json:"id"`
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
