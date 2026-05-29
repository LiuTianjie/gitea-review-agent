package gitea

import (
	"context"
	"strconv"

	"github.com/turning4th/codex-gitea/internal/model"
)

// reviewRequest is the POST body for submitting a review.
type reviewRequest struct {
	Event    string                `json:"event"`
	Body     string                `json:"body"`
	CommitID string                `json:"commit_id,omitempty"`
	Comments []model.ReviewComment `json:"comments,omitempty"`
}

// reviewResponse captures the fields we read back from review endpoints.
type reviewResponse struct {
	ID    int64     `json:"id"`
	State string    `json:"state"`
	Body  string    `json:"body"`
	Stale bool      `json:"stale"`
	User  *userInfo `json:"user"`
}

// userInfo is the embedded author object on a review.
type userInfo struct {
	Login    string `json:"login"`
	Username string `json:"username"`
}

// PostReview submits a review with optional inline comments. Each comment must
// carry exactly one of new_position / old_position (the other being 0), which
// the orchestrator derives from DiffMap.Position.
func (c *Client) PostReview(ctx context.Context, pr model.PRRef, commitID string, event model.ReviewEventType, body string, comments []model.ReviewComment) (int64, error) {
	path := c.repoPath(pr, "/pulls/"+strconv.Itoa(pr.Number)+"/reviews")
	req := reviewRequest{
		Event:    string(event),
		Body:     body,
		CommitID: commitID,
		Comments: comments,
	}
	var resp reviewResponse
	if err := c.doJSON(ctx, "POST", path, req, &resp); err != nil {
		return 0, err
	}
	return resp.ID, nil
}

// ListReviews returns existing reviews on the PR. Stale is populated from the
// API when present; the orchestrator decides bot-authored relevance itself.
func (c *Client) ListReviews(ctx context.Context, pr model.PRRef) ([]model.GiteaReview, error) {
	path := c.repoPath(pr, "/pulls/"+strconv.Itoa(pr.Number)+"/reviews")
	var resp []reviewResponse
	if err := c.doJSON(ctx, "GET", path, nil, &resp); err != nil {
		return nil, err
	}
	out := make([]model.GiteaReview, 0, len(resp))
	for _, r := range resp {
		out = append(out, model.GiteaReview{
			ID:    r.ID,
			State: r.State,
			Body:  r.Body,
			Stale: r.Stale,
		})
	}
	return out, nil
}

// dismissRequest is the POST body for dismissing a review.
type dismissRequest struct {
	Message string `json:"message"`
}

// DismissReview dismisses an existing review with an explanatory message.
func (c *Client) DismissReview(ctx context.Context, pr model.PRRef, reviewID int64, msg string) error {
	path := c.repoPath(pr, "/pulls/"+strconv.Itoa(pr.Number)+"/reviews/"+strconv.FormatInt(reviewID, 10)+"/dismissals")
	return c.doJSON(ctx, "POST", path, dismissRequest{Message: msg}, nil)
}
