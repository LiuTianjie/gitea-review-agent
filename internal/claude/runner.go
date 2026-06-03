package claude

import (
	"bytes"
	"context"
	_ "embed"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"syscall"
	"time"

	"github.com/turning4th/codex-gitea/internal/model"
)

const defaultTimeout = 30 * time.Minute
const readOnlyTools = "Read,Grep,Glob"

//go:embed findings.schema.json
var findingsSchema []byte

type Options struct {
	Bin               string
	Model             string
	APIKey            string
	BaseURL           string
	ClaudeHome        string
	CCSwitchBin       string
	CCSwitchConfigDir string
	CCSwitchProvider  string
	Timeout           time.Duration
}

type Runner struct {
	bin               string
	model             string
	apiKey            string
	baseURL           string
	claudeHome        string
	ccSwitchBin       string
	ccSwitchConfigDir string
	ccSwitchProvider  string
	timeout           time.Duration
}

var _ model.Reviewer = (*Runner)(nil)

func New(opts Options) *Runner {
	bin := opts.Bin
	if env := os.Getenv("CLAUDE_BIN"); env != "" {
		bin = env
	}
	if bin == "" {
		bin = "claude"
	}
	ccSwitchBin := opts.CCSwitchBin
	if ccSwitchBin == "" {
		ccSwitchBin = "cc-switch"
	}
	timeout := opts.Timeout
	if timeout <= 0 {
		timeout = defaultTimeout
	}
	return &Runner{
		bin:               bin,
		model:             opts.Model,
		apiKey:            opts.APIKey,
		baseURL:           opts.BaseURL,
		claudeHome:        opts.ClaudeHome,
		ccSwitchBin:       ccSwitchBin,
		ccSwitchConfigDir: opts.CCSwitchConfigDir,
		ccSwitchProvider:  opts.CCSwitchProvider,
		timeout:           timeout,
	}
}

func (r *Runner) Name() string { return "claude" }

func (r *Runner) Review(ctx context.Context, in model.CodexInput) (*model.ReviewResult, error) {
	if in.Worktree == "" {
		return nil, fmt.Errorf("claude review: empty worktree")
	}
	if err := r.applyProvider(ctx); err != nil {
		return nil, err
	}
	args := []string{
		"--print",
		"--output-format", "json",
		"--json-schema", string(findingsSchema),
		"--permission-mode", "dontAsk",
		"--allowedTools", readOnlyTools,
	}
	if r.model != "" {
		args = append(args, "--model", r.model)
	}
	if in.SessionID != "" {
		args = append(args, "--resume", in.SessionID)
	}
	args = append(args, buildReviewPrompt(in.BaseRef, in.Note, in.SessionID != ""))
	out, err := r.run(ctx, in.Worktree, r.bin, args, "")
	if err != nil {
		return nil, err
	}
	result, sessionID, err := parseReviewOutput(out)
	if err != nil {
		return nil, err
	}
	if sessionID != "" {
		result.SessionID = sessionID
	} else {
		result.SessionID = in.SessionID
	}
	return result, nil
}

func (r *Runner) Ask(ctx context.Context, sessionID, worktree, question string) (string, error) {
	if sessionID == "" {
		return "", fmt.Errorf("claude ask: empty session id")
	}
	if err := r.applyProvider(ctx); err != nil {
		return "", err
	}
	args := []string{
		"--print",
		"--output-format", "json",
		"--permission-mode", "dontAsk",
		"--allowedTools", readOnlyTools,
	}
	if r.model != "" {
		args = append(args, "--model", r.model)
	}
	args = append(args, "--resume", sessionID, buildAskPrompt(question))
	out, err := r.run(ctx, worktree, r.bin, args, "")
	if err != nil {
		return "", err
	}
	return parseTextOutput(out)
}

func (r *Runner) Status(ctx context.Context) (string, error) {
	parts := []string{}
	failures := []string{}
	requireProvider := strings.TrimSpace(r.ccSwitchProvider) != ""
	if requireProvider {
		if current, err := r.run(ctx, "", r.ccSwitchBin, []string{"--app", "claude", "provider", "current"}, ""); err == nil {
			parts = append(parts, "cc-switch current:\n"+strings.TrimSpace(string(current)))
		} else {
			parts = append(parts, "cc-switch current failed: "+err.Error())
			failures = append(failures, "cc-switch current: "+err.Error())
		}
		if envCheck, err := r.run(ctx, "", r.ccSwitchBin, []string{"--app", "claude", "env", "check"}, ""); err == nil {
			parts = append(parts, "cc-switch env:\n"+strings.TrimSpace(string(envCheck)))
		} else {
			parts = append(parts, "cc-switch env failed: "+err.Error())
			failures = append(failures, "cc-switch env: "+err.Error())
		}
	} else {
		parts = append(parts, "cc-switch: skipped (provider id not configured; using Claude API env)")
	}
	if auth, err := r.run(ctx, "", r.bin, []string{"auth", "status"}, ""); err == nil {
		parts = append(parts, "claude auth:\n"+strings.TrimSpace(string(auth)))
	} else {
		parts = append(parts, "claude auth failed: "+err.Error())
		failures = append(failures, "claude auth: "+err.Error())
	}
	status := strings.Join(parts, "\n\n")
	if len(failures) > 0 {
		return status, fmt.Errorf("%s", strings.Join(failures, "; "))
	}
	return status, nil
}

func (r *Runner) applyProvider(ctx context.Context) error {
	if strings.TrimSpace(r.ccSwitchProvider) == "" {
		return nil
	}
	_, err := r.run(ctx, "", r.ccSwitchBin, []string{"--app", "claude", "provider", "switch", r.ccSwitchProvider}, "")
	if err != nil {
		return fmt.Errorf("cc-switch provider switch: %w", err)
	}
	return nil
}

func (r *Runner) env() []string {
	out := make([]string, 0, len(os.Environ())+4)
	drop := map[string]bool{
		"GITEA_TOKEN": true, "GITEA_API_TOKEN": true, "GITEA_PASSWORD": true,
		"OPENAI_API_KEY": true, "OPENAI_TOKEN": true,
	}
	for _, kv := range os.Environ() {
		eq := strings.IndexByte(kv, '=')
		if eq < 0 || drop[kv[:eq]] {
			continue
		}
		out = append(out, kv)
	}
	if r.claudeHome != "" {
		out = append(out, "HOME="+r.claudeHome)
		out = append(out, "CLAUDE_CONFIG_DIR="+r.claudeHome)
	}
	if r.ccSwitchConfigDir != "" {
		out = append(out, "CC_SWITCH_CONFIG_DIR="+r.ccSwitchConfigDir)
	}
	if r.apiKey != "" {
		out = append(out, "ANTHROPIC_API_KEY="+r.apiKey)
	}
	if r.baseURL != "" {
		out = append(out, "ANTHROPIC_BASE_URL="+r.baseURL)
	}
	return out
}

func (r *Runner) run(ctx context.Context, dir, bin string, args []string, stdin string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, r.timeout)
	defer cancel()
	var stdout, stderr bytes.Buffer
	cmd := exec.CommandContext(ctx, bin, args...)
	if dir != "" {
		cmd.Dir = dir
	}
	cmd.Env = r.env()
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if stdin != "" {
		cmd.Stdin = strings.NewReader(stdin)
	}
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error { return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL) }
	cmd.WaitDelay = 2 * time.Second
	if err := cmd.Run(); err != nil {
		if ctx.Err() == context.DeadlineExceeded {
			return nil, fmt.Errorf("%s timed out after %s", bin, r.timeout)
		}
		msg := formatCommandFailure(stderr.String(), stdout.String())
		if msg != "" {
			return nil, fmt.Errorf("%s failed: %w: %s", bin, err, msg)
		}
		return nil, fmt.Errorf("%s failed: %w", bin, err)
	}
	return stdout.Bytes(), nil
}

func parseReviewOutput(out []byte) (*model.ReviewResult, string, error) {
	var direct model.ReviewResult
	if err := json.Unmarshal(bytes.TrimSpace(out), &direct); err == nil && (direct.Summary != "" || len(direct.Findings) > 0 || direct.OverallSeverity != "") {
		return &direct, "", nil
	}
	var wrapped struct {
		Result    any    `json:"result"`
		SessionID string `json:"session_id"`
	}
	if err := json.Unmarshal(bytes.TrimSpace(out), &wrapped); err != nil {
		return nil, "", fmt.Errorf("claude review: parse output: %w", err)
	}
	switch v := wrapped.Result.(type) {
	case string:
		if err := json.Unmarshal([]byte(strings.TrimSpace(v)), &direct); err != nil {
			return nil, "", fmt.Errorf("claude review: parse result string: %w", err)
		}
	case map[string]any:
		data, _ := json.Marshal(v)
		if err := json.Unmarshal(data, &direct); err != nil {
			return nil, "", fmt.Errorf("claude review: parse result object: %w", err)
		}
	default:
		return nil, "", fmt.Errorf("claude review: missing structured result")
	}
	return &direct, wrapped.SessionID, nil
}

func parseTextOutput(out []byte) (string, error) {
	var wrapped struct {
		Result string `json:"result"`
	}
	if err := json.Unmarshal(bytes.TrimSpace(out), &wrapped); err == nil && strings.TrimSpace(wrapped.Result) != "" {
		return strings.TrimSpace(wrapped.Result), nil
	}
	return strings.TrimSpace(string(out)), nil
}

func formatCommandFailure(stderr, stdout string) string {
	stderr = strings.TrimSpace(stderr)
	stdout = strings.TrimSpace(stdout)
	switch {
	case stderr != "" && stdout != "":
		return "stderr:\n" + stderr + "\n\nstdout:\n" + stdout
	case stderr != "":
		return stderr
	case stdout != "":
		return stdout
	default:
		return ""
	}
}

func buildReviewPrompt(baseRef, note string, resume bool) string {
	if baseRef == "" {
		baseRef = "main"
	}
	verb := "performing a STATIC review"
	if resume {
		verb = "re-reviewing a pull request STATICALLY"
	}
	var b strings.Builder
	fmt.Fprintf(&b, `You are a senior code reviewer %s.

Run `+"`git diff %s...HEAD`"+` yourself to obtain the full pull-request diff from the target branch to the current HEAD.
Use that diff as the entrypoint, and inspect related functions, types, imports, call sites, and nearby files when needed to understand impact.

Hard rules:
- Do NOT build, run, compile, or test the code.
- Do NOT modify any files.
- Only read-only inspection commands are allowed.
- Report only risks introduced or exposed by this PR.
- Write all human-facing output in Simplified Chinese by default.
- For every finding include path, line, side, severity, title, body, and a tags array with zero to six short free-form tags.
- Include resolved_fingerprints for prior findings that are clearly fixed.

Return ONLY JSON conforming to the provided schema.`, verb, baseRef)
	if strings.TrimSpace(note) != "" {
		fmt.Fprintf(&b, "\n\nAdditional context:\n%s", note)
	}
	return b.String()
}

func buildAskPrompt(question string) string {
	return "用简体中文回答这个 PR review follow-up。不要修改文件。\n\n问题：\n" + strings.TrimSpace(question)
}
