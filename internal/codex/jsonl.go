package codex

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
)

// maxStreamLine bounds a single JSONL line. codex emits very large lines
// (aggregated command output can be tens of KB), so the default 64KB scanner
// buffer is insufficient.
const maxStreamLine = 16 * 1024 * 1024

// streamEvent is the subset of codex `--json` event fields we consume.
// Each line of the stream is one JSON object.
type streamEvent struct {
	Type     string `json:"type"`
	ThreadID string `json:"thread_id"`
	Item     *struct {
		Type string `json:"type"`
		Text string `json:"text"`
	} `json:"item"`
}

// streamResult is what we extract from a codex event stream.
type streamResult struct {
	// ThreadID is the codex session id from the thread.started event.
	ThreadID string
	// LastAgentMessage is the text of the final agent_message item (used by Ask).
	LastAgentMessage string
}

// parseStream scans a codex `--json` event stream (one JSON object per line),
// extracting the thread_id and the last agent_message text. Non-JSON lines
// (stray warnings) are tolerated and skipped.
func parseStream(r io.Reader) (streamResult, error) {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 64*1024), maxStreamLine)

	var res streamResult
	for sc.Scan() {
		line := bytes.TrimSpace(sc.Bytes())
		if len(line) == 0 || line[0] != '{' {
			continue
		}
		var ev streamEvent
		if err := json.Unmarshal(line, &ev); err != nil {
			// Tolerate non-event output interleaved on stdout.
			continue
		}
		switch ev.Type {
		case "thread.started":
			if ev.ThreadID != "" {
				res.ThreadID = ev.ThreadID
			}
		case "item.completed":
			if ev.Item != nil && ev.Item.Type == "agent_message" {
				res.LastAgentMessage = ev.Item.Text
			}
		}
	}
	if err := sc.Err(); err != nil {
		return res, fmt.Errorf("scan codex stream: %w", err)
	}
	return res, nil
}
