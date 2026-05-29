// Package gitea implements model.GiteaClient against the Gitea REST API.
package gitea

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/turning4th/codex-gitea/internal/model"
)

// Client talks to a Gitea instance's REST API (/api/v1).
type Client struct {
	baseURL string
	token   string
	http    *http.Client
}

// compile-time guarantee that *Client satisfies the port.
var _ model.GiteaClient = (*Client)(nil)

// New builds a Client. baseURL is the instance root (e.g.
// "https://gitea.example.com"); token is a personal/bot token used in the
// "Authorization: token <token>" header. If httpClient is nil a client with a
// sane default timeout is used.
func New(baseURL, token string, httpClient *http.Client) *Client {
	if httpClient == nil {
		httpClient = &http.Client{Timeout: 30 * time.Second}
	}
	return &Client{
		baseURL: strings.TrimRight(baseURL, "/"),
		token:   token,
		http:    httpClient,
	}
}

// repoPath builds an /api/v1/repos/{owner}/{repo}{suffix} path with each
// path segment escaped. suffix must begin with "/" (or be "").
func (c *Client) repoPath(pr model.PRRef, suffix string) string {
	return fmt.Sprintf("/api/v1/repos/%s/%s%s",
		url.PathEscape(pr.Owner), url.PathEscape(pr.Repo), suffix)
}

// doJSON sends an optional JSON body and decodes an optional JSON response.
// It sets auth + content headers and turns any non-2xx status into an error
// that includes the response body for diagnostics.
func (c *Client) doJSON(ctx context.Context, method, path string, in, out any) error {
	var body io.Reader
	if in != nil {
		b, err := json.Marshal(in)
		if err != nil {
			return fmt.Errorf("gitea: marshal request: %w", err)
		}
		body = bytes.NewReader(b)
	}

	req, err := http.NewRequestWithContext(ctx, method, c.baseURL+path, body)
	if err != nil {
		return fmt.Errorf("gitea: build request: %w", err)
	}
	req.Header.Set("Authorization", "token "+c.token)
	req.Header.Set("Accept", "application/json")
	if in != nil {
		req.Header.Set("Content-Type", "application/json")
	}

	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("gitea: %s %s: %w", method, path, err)
	}
	defer resp.Body.Close()

	respBody, _ := io.ReadAll(resp.Body)
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("gitea: %s %s: status %d: %s",
			method, path, resp.StatusCode, strings.TrimSpace(string(respBody)))
	}

	if out != nil && len(respBody) > 0 {
		if err := json.Unmarshal(respBody, out); err != nil {
			return fmt.Errorf("gitea: decode %s %s response: %w", method, path, err)
		}
	}
	return nil
}

// doRaw fetches a non-JSON resource (e.g. a .diff) and returns the raw body.
func (c *Client) doRaw(ctx context.Context, method, path string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, method, c.baseURL+path, nil)
	if err != nil {
		return nil, fmt.Errorf("gitea: build request: %w", err)
	}
	req.Header.Set("Authorization", "token "+c.token)

	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("gitea: %s %s: %w", method, path, err)
	}
	defer resp.Body.Close()

	respBody, _ := io.ReadAll(resp.Body)
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("gitea: %s %s: status %d: %s",
			method, path, resp.StatusCode, strings.TrimSpace(string(respBody)))
	}
	return respBody, nil
}
