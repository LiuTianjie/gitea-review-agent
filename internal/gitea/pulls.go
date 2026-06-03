package gitea

import (
	"context"
	"strconv"

	"github.com/turning4th/codex-gitea/internal/model"
)

type pullResponse struct {
	State  string `json:"state"`
	Merged bool   `json:"merged"`
}

// GetPullRequestStatus returns the live PR state. Review submission is only
// valid while state is open and merged is false.
func (c *Client) GetPullRequestStatus(ctx context.Context, pr model.PRRef) (model.PullRequestStatus, error) {
	path := c.repoPath(pr, "/pulls/"+strconv.Itoa(pr.Number))
	var resp pullResponse
	if err := c.doJSON(ctx, "GET", path, nil, &resp); err != nil {
		return model.PullRequestStatus{}, err
	}
	return model.PullRequestStatus{State: resp.State, Merged: resp.Merged}, nil
}
