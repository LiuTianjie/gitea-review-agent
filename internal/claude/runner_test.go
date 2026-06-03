package claude

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/turning4th/codex-gitea/internal/model"
)

func writeClaudeStub(t *testing.T, logPath string) string {
	t.Helper()
	dir := t.TempDir()
	script := filepath.Join(dir, "claude-stub.sh")
	body := `#!/bin/sh
if [ -n "$STUB_SLEEP" ]; then
  sleep "$STUB_SLEEP"
fi
{
  echo "CWD=$(pwd)"
  echo "HOME=${HOME}"
  echo "CC_SWITCH_CONFIG_DIR=${CC_SWITCH_CONFIG_DIR}"
  echo "ANTHROPIC_API_KEY=${ANTHROPIC_API_KEY}"
  echo "ANTHROPIC_BASE_URL=${ANTHROPIC_BASE_URL}"
  echo "GITEA_TOKEN=${GITEA_TOKEN}"
  i=0
  for a in "$@"; do
    echo "ARG[$i]=$a"
    i=$((i+1))
  done
} >> "$STUB_LOG"
if [ "$1" = "auth" ]; then
  echo "Claude auth ok"
  exit 0
fi
cat <<'JSON'
{"type":"result","session_id":"claude-session-1","result":"{\"summary\":\"claude summary\",\"overall_severity\":\"high\",\"findings\":[{\"path\":\"calc.go\",\"line\":19,\"side\":\"NEW\",\"severity\":\"high\",\"title\":\"bad math\",\"body\":\"boom\",\"tags\":[\"runtime\"]}],\"resolved_fingerprints\":[]}"}
JSON
`
	if err := os.WriteFile(script, []byte(body), 0o755); err != nil {
		t.Fatalf("write stub: %v", err)
	}
	t.Setenv("STUB_LOG", logPath)
	return script
}

func TestRunnerReview(t *testing.T) {
	logPath := filepath.Join(t.TempDir(), "claude.log")
	bin := writeClaudeStub(t, logPath)
	t.Setenv("GITEA_TOKEN", "secret")

	worktree := t.TempDir()
	home := t.TempDir()
	ccDir := t.TempDir()
	r := New(Options{Bin: bin, ClaudeHome: home, CCSwitchConfigDir: ccDir, Model: "sonnet", APIKey: "ak-claude", BaseURL: "https://relay.example"})
	res, err := r.Review(context.Background(), model.CodexInput{Worktree: worktree, BaseRef: "main"})
	if err != nil {
		t.Fatalf("Review: %v", err)
	}
	if res.SessionID != "claude-session-1" || res.Summary != "claude summary" {
		t.Fatalf("unexpected result: %+v", res)
	}
	if len(res.Findings) != 1 || res.Findings[0].Tags[0] != "runtime" {
		t.Fatalf("finding tags not parsed: %+v", res.Findings)
	}
	raw, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatalf("read log: %v", err)
	}
	log := string(raw)
	for _, want := range []string{
		"ARG[0]=--print",
		"--output-format",
		"--json-schema",
		"--permission-mode",
		"--allowedTools",
		"--model",
		"sonnet",
		"git diff main...HEAD",
		"HOME=" + home,
		"CC_SWITCH_CONFIG_DIR=" + ccDir,
		"ANTHROPIC_API_KEY=ak-claude",
		"ANTHROPIC_BASE_URL=https://relay.example",
		"GITEA_TOKEN=\n",
	} {
		if !strings.Contains(log, want) {
			t.Fatalf("log missing %q:\n%s", want, log)
		}
	}
	if strings.Contains(log, "Bash(") {
		t.Fatalf("review should not allow shell tools:\n%s", log)
	}
	if !strings.Contains(log, "Read,Grep,Glob") {
		t.Fatalf("review should allow only read-only tools:\n%s", log)
	}
}

func TestRunnerReviewTimeout(t *testing.T) {
	bin := writeClaudeStub(t, filepath.Join(t.TempDir(), "claude.log"))
	t.Setenv("STUB_SLEEP", "5")
	r := New(Options{Bin: bin, Timeout: 100 * time.Millisecond})
	_, err := r.Review(context.Background(), model.CodexInput{Worktree: t.TempDir(), BaseRef: "main"})
	if err == nil || !strings.Contains(err.Error(), "timed out") {
		t.Fatalf("err = %v, want timeout", err)
	}
}

func TestRunnerStatusReturnsErrorWhenChecksFail(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "missing-claude")
	r := New(Options{Bin: missing, CCSwitchBin: missing, Timeout: 100 * time.Millisecond})
	status, err := r.Status(context.Background())
	if err == nil {
		t.Fatalf("Status err = nil, want failure")
	}
	if !strings.Contains(status, "cc-switch: skipped") || !strings.Contains(status, "claude auth failed") {
		t.Fatalf("status missing failure details:\n%s", status)
	}
}

func TestRunnerStatusDoesNotRequireCCSwitchWithoutProvider(t *testing.T) {
	logPath := filepath.Join(t.TempDir(), "claude.log")
	bin := writeClaudeStub(t, logPath)
	t.Setenv("STUB_SLEEP", "")
	missing := filepath.Join(t.TempDir(), "missing-cc-switch")
	r := New(Options{Bin: bin, CCSwitchBin: missing, Timeout: time.Second})
	status, err := r.Status(context.Background())
	if err != nil {
		t.Fatalf("Status err = %v, want nil when cc-switch provider is not configured\n%s", err, status)
	}
	if !strings.Contains(status, "cc-switch: skipped") || !strings.Contains(status, "claude auth") {
		t.Fatalf("status missing optional cc-switch/auth details:\n%s", status)
	}
}

func TestRunnerStatusRequiresCCSwitchWhenProviderConfigured(t *testing.T) {
	logPath := filepath.Join(t.TempDir(), "claude.log")
	bin := writeClaudeStub(t, logPath)
	t.Setenv("STUB_SLEEP", "")
	missing := filepath.Join(t.TempDir(), "missing-cc-switch")
	r := New(Options{Bin: bin, CCSwitchBin: missing, CCSwitchProvider: "relay", Timeout: time.Second})
	status, err := r.Status(context.Background())
	if err == nil {
		t.Fatalf("Status err = nil, want cc-switch failure when provider is configured\n%s", status)
	}
}
