package codex

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"syscall"
	"time"

	"github.com/turning4th/codex-gitea/internal/model"
)

// defaultTimeout bounds a single codex invocation when none is configured.
const defaultTimeout = 30 * time.Minute

// secretEnvKeys are environment variables that must never reach the codex
// subprocess (and thus the worktree it inspects). The orchestrator's Gitea and
// OpenAI service tokens live here; codex authenticates via CODEX_HOME or its
// own CODEX_API_KEY, never the service tokens.
var secretEnvKeys = map[string]bool{
	"GITEA_TOKEN":         true,
	"GITEA_API_TOKEN":     true,
	"GITEA_PASSWORD":      true,
	"OPENAI_API_KEY":      true,
	"OPENAI_TOKEN":        true,
	"OPENAI_ORG_ID":       true,
	"OPENAI_ORGANIZATION": true,
	"ANTHROPIC_API_KEY":   true,
	// We always set CODEX_HOME / CODEX_API_KEY explicitly below, so drop any
	// inherited values to avoid surprises.
	"CODEX_HOME":    true,
	"CODEX_API_KEY": true,
}

// Options configures a Runner.
type Options struct {
	// Bin is the codex executable. Defaults to "codex"; the CODEX_BIN env var
	// overrides it (used by tests to point at a stub).
	Bin string
	// CodexHome sets CODEX_HOME for the codex process (auth/config dir).
	CodexHome string
	// Model optionally overrides the codex model (--model).
	Model string
	// APIKey, when set, runs codex in api-key mode (CODEX_API_KEY).
	APIKey string
	// SandboxMode is passed to codex as sandbox_mode. Defaults to read-only.
	SandboxMode string
	// Timeout bounds a single invocation. Defaults to 30m.
	Timeout time.Duration
}

// Runner invokes the codex CLI in headless mode to perform structured reviews.
type Runner struct {
	bin       string
	codexHome string
	model     string
	apiKey    string
	sandbox   string
	timeout   time.Duration
}

var _ model.CodexRunner = (*Runner)(nil)

func (r *Runner) Name() string { return "codex" }

// New builds a Runner from Options. CODEX_BIN overrides Options.Bin.
func New(opts Options) *Runner {
	bin := opts.Bin
	if env := os.Getenv("CODEX_BIN"); env != "" {
		bin = env
	}
	if bin == "" {
		bin = "codex"
	}
	timeout := opts.Timeout
	if timeout <= 0 {
		timeout = defaultTimeout
	}
	sandbox := opts.SandboxMode
	if strings.TrimSpace(sandbox) == "" {
		sandbox = "read-only"
	}
	return &Runner{
		bin:       bin,
		codexHome: opts.CodexHome,
		model:     opts.Model,
		apiKey:    opts.APIKey,
		sandbox:   sandbox,
		timeout:   timeout,
	}
}

// env builds the codex process environment: the parent environment minus any
// secret-bearing variables, plus CODEX_HOME and (in api-key mode) CODEX_API_KEY.
func (r *Runner) env() []string {
	out := make([]string, 0, len(os.Environ())+2)
	for _, kv := range os.Environ() {
		eq := strings.IndexByte(kv, '=')
		if eq < 0 {
			continue
		}
		if secretEnvKeys[kv[:eq]] {
			continue
		}
		out = append(out, kv)
	}
	if r.codexHome != "" {
		out = append(out, "CODEX_HOME="+r.codexHome)
	}
	if r.apiKey != "" {
		out = append(out, "CODEX_API_KEY="+r.apiKey)
	}
	return out
}

// reviewBaseArgs returns the flags shared by new and resume reviews.
func (r *Runner) reviewBaseArgs(schemaPath, outPath string) []string {
	args := []string{
		"--json",
		"--output-schema", schemaPath,
		"-o", outPath,
		"-c", "approval_policy=never",
		"-c", "sandbox_mode=" + r.sandbox,
		"--skip-git-repo-check",
	}
	if r.model != "" {
		args = append(args, "--model", r.model)
	}
	return args
}

// Review runs a structured review (new when in.SessionID is empty, otherwise a
// resume of that thread) and returns findings plus the codex session id.
func (r *Runner) Review(ctx context.Context, in model.CodexInput) (*model.ReviewResult, error) {
	if in.Worktree == "" {
		return nil, fmt.Errorf("codex review: empty worktree")
	}

	schemaPath, cleanupSchema, err := writeSchemaTemp()
	if err != nil {
		return nil, err
	}
	defer cleanupSchema()

	outFile, err := os.CreateTemp("", "codex-findings-*.json")
	if err != nil {
		return nil, fmt.Errorf("codex review: create out file: %w", err)
	}
	outPath := outFile.Name()
	outFile.Close()
	defer os.Remove(outPath)

	var args []string
	var prompt string
	if in.SessionID == "" {
		prompt = buildReviewPrompt(in.BaseRef, in.Note)
		args = append([]string{"exec", "-"}, r.reviewBaseArgs(schemaPath, outPath)...)
	} else {
		prompt = buildResumePrompt(in.BaseRef, in.Note)
		args = append([]string{"exec", "resume", in.SessionID, "-"}, r.reviewBaseArgs(schemaPath, outPath)...)
	}

	stream, err := r.run(ctx, in.Worktree, args, prompt)
	if err != nil {
		return nil, err
	}

	sr, err := parseStream(bytes.NewReader(stream))
	if err != nil {
		return nil, err
	}

	data, err := os.ReadFile(outPath)
	if err != nil {
		return nil, fmt.Errorf("codex review: read output file: %w", err)
	}
	if len(bytes.TrimSpace(data)) == 0 {
		return nil, fmt.Errorf("codex review: empty output file (no structured result produced)")
	}

	var result model.ReviewResult
	if err := json.Unmarshal(data, &result); err != nil {
		return nil, fmt.Errorf("codex review: parse output file: %w", err)
	}
	normalizeReviewResult(&result)

	result.SessionID = sr.ThreadID
	if result.SessionID == "" {
		// Fall back to the input session id on resume if the stream omitted it.
		result.SessionID = in.SessionID
	}
	return &result, nil
}

// Ask resumes a session with a free-form question and returns the agent's text.
func (r *Runner) Ask(ctx context.Context, sessionID, worktree, question string) (string, error) {
	if sessionID == "" {
		return "", fmt.Errorf("codex ask: empty session id")
	}
	prompt := buildAskPrompt(question)
	args := []string{
		"exec", "resume", sessionID, "-",
		"--json",
		"-c", "approval_policy=never",
		"-c", "sandbox_mode=" + r.sandbox,
		"--skip-git-repo-check",
	}
	if r.model != "" {
		args = append(args, "--model", r.model)
	}

	stream, err := r.run(ctx, worktree, args, prompt)
	if err != nil {
		return "", err
	}
	sr, err := parseStream(bytes.NewReader(stream))
	if err != nil {
		return "", err
	}
	if sr.LastAgentMessage == "" {
		return "", fmt.Errorf("codex ask: no agent message in response")
	}
	return humanizeStructuredAnswer(sr.LastAgentMessage), nil
}

// Status reports codex auth state by running `codex login status`.
func (r *Runner) Status(ctx context.Context) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, r.timeout)
	defer cancel()

	cmd := exec.CommandContext(ctx, r.bin, "login", "status")
	cmd.Env = r.env()
	out, err := cmd.CombinedOutput()
	text := strings.TrimSpace(string(out))
	if err != nil {
		if text != "" {
			return text, fmt.Errorf("codex login status: %w: %s", err, text)
		}
		return "", fmt.Errorf("codex login status: %w", err)
	}
	return text, nil
}

// run executes codex in dir, returns captured stdout, and surfaces stderr on
// failure. ctx is bounded by the runner timeout.
func (r *Runner) run(ctx context.Context, dir string, args []string, stdin string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, r.timeout)
	defer cancel()

	var stdout, stderr bytes.Buffer
	cmd := exec.CommandContext(ctx, r.bin, args...)
	cmd.Dir = dir
	cmd.Env = r.env()
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if stdin != "" {
		cmd.Stdin = strings.NewReader(stdin)
	}

	// Run codex in its own process group so a timeout kills the whole tree.
	// codex spawns children (git, etc.); killing only the direct child leaves
	// grandchildren holding the stdout pipe open, which makes cmd.Wait block
	// past the deadline (the I/O copy never sees EOF).
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error {
		// Negative pid targets the entire process group (group leader == child pid).
		return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
	}
	// Backstop: if a grandchild still holds the pipes after the group kill,
	// abort the I/O copy so Wait returns promptly instead of hanging.
	cmd.WaitDelay = 2 * time.Second

	err := cmd.Run()
	if err != nil {
		if ctxErr := ctx.Err(); ctxErr == context.DeadlineExceeded {
			return nil, fmt.Errorf("codex timed out after %s: %w", r.timeout, ctxErr)
		}
		msg := formatCommandFailure(stderr.String(), stdout.String())
		if msg != "" {
			return nil, fmt.Errorf("codex exec failed: %w: %s", err, msg)
		}
		return nil, fmt.Errorf("codex exec failed: %w", err)
	}
	return stdout.Bytes(), nil
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

func normalizeReviewResult(result *model.ReviewResult) {
	if result == nil {
		return
	}
	trimmed := strings.TrimSpace(result.Summary)
	if !strings.HasPrefix(trimmed, "{") {
		return
	}
	var nested model.ReviewResult
	if err := json.Unmarshal([]byte(trimmed), &nested); err != nil {
		return
	}
	if strings.TrimSpace(nested.Summary) == "" && len(nested.Findings) == 0 && nested.OverallSeverity == "" {
		return
	}
	sessionID := result.SessionID
	*result = nested
	result.SessionID = sessionID
}

func humanizeStructuredAnswer(text string) string {
	trimmed := strings.TrimSpace(text)
	if !strings.HasPrefix(trimmed, "{") {
		return text
	}
	var result model.ReviewResult
	if err := json.Unmarshal([]byte(trimmed), &result); err != nil {
		return text
	}
	if strings.TrimSpace(result.Summary) == "" && len(result.Findings) == 0 && result.OverallSeverity == "" {
		return text
	}
	var b strings.Builder
	b.WriteString(result.Summary)
	if result.OverallSeverity != "" {
		fmt.Fprintf(&b, "\n\n整体严重程度：**%s**", result.OverallSeverity)
	}
	if len(result.Findings) > 0 {
		b.WriteString("\n\n需要关注的问题：\n")
		for _, f := range result.Findings {
			fmt.Fprintf(&b, "- **[%s] %s** (`%s:%d`): %s\n",
				strings.ToUpper(string(f.Severity)), f.Title, f.Path, f.Line, f.Body)
		}
	}
	return strings.TrimSpace(b.String())
}
