package codex

import (
	"os"
	"strings"
	"testing"
)

func TestParseStream_StreamSample(t *testing.T) {
	f, err := os.Open("../../testdata/codex-stream-sample.jsonl")
	if err != nil {
		t.Fatalf("open sample: %v", err)
	}
	defer f.Close()

	res, err := parseStream(f)
	if err != nil {
		t.Fatalf("parseStream: %v", err)
	}

	wantThread := "019e7202-93b1-7f13-9920-fb8aefd331b9"
	if res.ThreadID != wantThread {
		t.Errorf("thread_id = %q, want %q", res.ThreadID, wantThread)
	}
	// The last agent_message in the stream is the final structured result.
	if !strings.Contains(res.LastAgentMessage, "Loop condition indexes one past the slice bounds") {
		t.Errorf("last agent_message did not contain expected text, got: %q", res.LastAgentMessage)
	}
	// It must be the LAST one, not an earlier interim message.
	if strings.Contains(res.LastAgentMessage, "I’ll inspect the requested") {
		t.Errorf("last agent_message returned an interim message instead of the final one")
	}
}

func TestParseStream_ResumeSample(t *testing.T) {
	f, err := os.Open("../../testdata/codex-resume-sample.jsonl")
	if err != nil {
		t.Fatalf("open resume sample: %v", err)
	}
	defer f.Close()

	res, err := parseStream(f)
	if err != nil {
		t.Fatalf("parseStream: %v", err)
	}

	// resume keeps the same thread_id.
	wantThread := "019e7202-93b1-7f13-9920-fb8aefd331b9"
	if res.ThreadID != wantThread {
		t.Errorf("thread_id = %q, want %q", res.ThreadID, wantThread)
	}
	if !strings.HasPrefix(res.LastAgentMessage, "Yes. Calling") {
		t.Errorf("resume agent_message = %q, want it to start with 'Yes. Calling'", res.LastAgentMessage)
	}
}

func TestParseStream_ToleratesNonJSON(t *testing.T) {
	in := strings.NewReader(strings.Join([]string{
		"warning: some stray stderr leaked to stdout",
		`{"type":"thread.started","thread_id":"abc-123"}`,
		"",
		`{"type":"item.completed","item":{"type":"agent_message","text":"hello"}}`,
		"not json at all",
	}, "\n"))

	res, err := parseStream(in)
	if err != nil {
		t.Fatalf("parseStream: %v", err)
	}
	if res.ThreadID != "abc-123" {
		t.Errorf("thread_id = %q, want abc-123", res.ThreadID)
	}
	if res.LastAgentMessage != "hello" {
		t.Errorf("agent_message = %q, want hello", res.LastAgentMessage)
	}
}

func TestParseStream_LargeLine(t *testing.T) {
	// A command_execution line with a huge aggregated_output must not blow the
	// scanner buffer, and the thread_id must still be extracted.
	big := strings.Repeat("x", 2*1024*1024)
	in := strings.NewReader(strings.Join([]string{
		`{"type":"thread.started","thread_id":"big-1"}`,
		`{"type":"item.completed","item":{"type":"command_execution","text":"` + big + `"}}`,
		`{"type":"item.completed","item":{"type":"agent_message","text":"done"}}`,
	}, "\n"))

	res, err := parseStream(in)
	if err != nil {
		t.Fatalf("parseStream: %v", err)
	}
	if res.ThreadID != "big-1" {
		t.Errorf("thread_id = %q, want big-1", res.ThreadID)
	}
	if res.LastAgentMessage != "done" {
		t.Errorf("agent_message = %q, want done", res.LastAgentMessage)
	}
}
