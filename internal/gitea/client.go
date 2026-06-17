// Package gitea implements model.GiteaClient against the Gitea REST API.
package gitea

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
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
	config  ConfigFunc
}

// compile-time guarantee that *Client satisfies the port.
var _ model.GiteaClient = (*Client)(nil)

type Config struct {
	BaseURL string
	Token   string
	Timeout time.Duration
}

type ConfigFunc func() Config

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

// NewDynamic builds a client that reads URL, token, and timeout before each
// request. It lets admin-console settings take effect without restarting.
func NewDynamic(fn ConfigFunc) *Client {
	return &Client{config: fn}
}

func (c *Client) requestConfig() (string, string, *http.Client) {
	if c.config != nil {
		cfg := c.config()
		timeout := cfg.Timeout
		if timeout <= 0 {
			timeout = 30 * time.Second
		}
		return strings.TrimRight(cfg.BaseURL, "/"), cfg.Token, &http.Client{Timeout: timeout}
	}
	httpClient := c.http
	if httpClient == nil {
		httpClient = &http.Client{Timeout: 30 * time.Second}
	}
	return c.baseURL, c.token, httpClient
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
	baseURL, token, httpClient := c.requestConfig()
	var bodyBytes []byte
	if in != nil {
		var err error
		bodyBytes, err = json.Marshal(in)
		if err != nil {
			return fmt.Errorf("gitea: marshal request: %w", err)
		}
	}

	attempts := requestAttempts(method)
	var lastErr error
	for attempt := 1; attempt <= attempts; attempt++ {
		var body io.Reader
		if bodyBytes != nil {
			body = bytes.NewReader(bodyBytes)
		}
		req, err := http.NewRequestWithContext(ctx, method, baseURL+path, body)
		if err != nil {
			return fmt.Errorf("gitea: build request: %w", err)
		}
		req.Header.Set("Authorization", "token "+token)
		req.Header.Set("Accept", "application/json")
		if in != nil {
			req.Header.Set("Content-Type", "application/json")
		}

		resp, err := httpClient.Do(req)
		if err != nil {
			lastErr = fmt.Errorf("gitea: %s %s: %w", method, path, err)
			if attempt < attempts && shouldRetryRequest(ctx, err, 0) {
				waitBeforeRetry(ctx)
				continue
			}
			return lastErr
		}

		respBody, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode < 200 || resp.StatusCode >= 300 {
			lastErr = fmt.Errorf("gitea: %s %s: status %d: %s",
				method, path, resp.StatusCode, strings.TrimSpace(string(respBody)))
			if attempt < attempts && shouldRetryRequest(ctx, nil, resp.StatusCode) {
				waitBeforeRetry(ctx)
				continue
			}
			return lastErr
		}

		if out != nil && len(respBody) > 0 {
			if err := json.Unmarshal(respBody, out); err != nil {
				return fmt.Errorf("gitea: decode %s %s response: %w", method, path, err)
			}
		}
		return nil
	}
	return lastErr
}

// doRaw fetches a non-JSON resource (e.g. a .diff) and returns the raw body.
func (c *Client) doRaw(ctx context.Context, method, path string) ([]byte, error) {
	baseURL, token, httpClient := c.requestConfig()
	attempts := requestAttempts(method)
	var lastErr error
	for attempt := 1; attempt <= attempts; attempt++ {
		req, err := http.NewRequestWithContext(ctx, method, baseURL+path, nil)
		if err != nil {
			return nil, fmt.Errorf("gitea: build request: %w", err)
		}
		req.Header.Set("Authorization", "token "+token)

		resp, err := httpClient.Do(req)
		if err != nil {
			lastErr = fmt.Errorf("gitea: %s %s: %w", method, path, err)
			if attempt < attempts && shouldRetryRequest(ctx, err, 0) {
				waitBeforeRetry(ctx)
				continue
			}
			return nil, lastErr
		}

		respBody, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode < 200 || resp.StatusCode >= 300 {
			lastErr = fmt.Errorf("gitea: %s %s: status %d: %s",
				method, path, resp.StatusCode, strings.TrimSpace(string(respBody)))
			if attempt < attempts && shouldRetryRequest(ctx, nil, resp.StatusCode) {
				waitBeforeRetry(ctx)
				continue
			}
			return nil, lastErr
		}
		return respBody, nil
	}
	return nil, lastErr
}

func requestAttempts(method string) int {
	if method == http.MethodGet {
		return 2
	}
	return 1
}

func shouldRetryRequest(ctx context.Context, err error, status int) bool {
	if ctx.Err() != nil {
		return false
	}
	if status == http.StatusTooManyRequests || status == http.StatusBadGateway ||
		status == http.StatusServiceUnavailable || status == http.StatusGatewayTimeout {
		return true
	}
	if err == nil {
		return false
	}
	if errors.Is(err, context.Canceled) {
		return false
	}
	var netErr net.Error
	if errors.As(err, &netErr) && netErr.Timeout() {
		return true
	}
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "timeout") ||
		strings.Contains(msg, "deadline exceeded") ||
		strings.Contains(msg, "connection reset") ||
		strings.Contains(msg, "connection refused")
}

func waitBeforeRetry(ctx context.Context) {
	timer := time.NewTimer(500 * time.Millisecond)
	defer timer.Stop()
	select {
	case <-timer.C:
	case <-ctx.Done():
	}
}
