package gitea

import (
	"bufio"
	"context"
	"strconv"
	"strings"

	"github.com/turning4th/codex-gitea/internal/model"
)

// GetDiff fetches the PR's unified diff and parses it into a DiffMap recording
// which new-file and old-file line numbers fall inside the diff hunks.
func (c *Client) GetDiff(ctx context.Context, pr model.PRRef) (model.DiffMap, error) {
	path := c.repoPath(pr, "/pulls/"+strconv.Itoa(pr.Number)+".diff")
	raw, err := c.doRaw(ctx, "GET", path)
	if err != nil {
		return model.DiffMap{}, err
	}
	return ParseUnifiedDiff(string(raw)), nil
}

// ParseUnifiedDiff parses a standard unified diff (as produced by `git diff` /
// Gitea's `.diff` endpoint) into a DiffMap.
//
// For each file it records:
//   - NewLines: new-file line numbers for context (" ") and added ("+") lines.
//   - OldLines: old-file line numbers for context (" ") and removed ("-") lines.
//
// Hunk headers look like "@@ -oldStart,oldCount +newStart,newCount @@". Counts
// are optional (default 1). File boundaries are detected via "diff --git" and
// the canonical path is taken from the "+++ b/<path>" line (falling back to
// "--- a/<path>" for deletions).
func ParseUnifiedDiff(diff string) model.DiffMap {
	dm := model.DiffMap{Files: map[string]model.FileDiff{}}

	var (
		cur          *model.FileDiff // current file's accumulator
		oldLine      int             // next old-file line number
		newLine      int             // next new-file line number
		inHunk       bool
		pendingMinus string // "--- a/..." path seen but not yet committed
	)

	// fileFor returns (creating if needed) the FileDiff for path and makes it
	// current.
	fileFor := func(path string) {
		fd, ok := dm.Files[path]
		if !ok {
			fd = model.FileDiff{NewLines: map[int]bool{}, OldLines: map[int]bool{}}
			dm.Files[path] = fd
		}
		cur = &fd
		// Note: FileDiff holds maps, so the copy shares the underlying maps;
		// writes through cur are visible in dm.Files[path].
	}

	sc := bufio.NewScanner(strings.NewReader(diff))
	// Allow long lines (default token cap is 64KiB).
	sc.Buffer(make([]byte, 0, 64*1024), 16*1024*1024)

	for sc.Scan() {
		line := sc.Text()

		switch {
		case strings.HasPrefix(line, "diff --git"):
			// Start of a new file section; reset state until we learn the path.
			cur = nil
			inHunk = false
			pendingMinus = ""

		case strings.HasPrefix(line, "--- "):
			pendingMinus = stripDiffPath(line[len("--- "):])
			inHunk = false

		case strings.HasPrefix(line, "+++ "):
			p := stripDiffPath(line[len("+++ "):])
			if p == "" || p == "/dev/null" {
				// Deletion: file has no new side; key by the old path.
				p = pendingMinus
			}
			if p != "" {
				fileFor(p)
			}
			inHunk = false

		case strings.HasPrefix(line, "@@"):
			os, ns, ok := parseHunkHeader(line)
			if !ok || cur == nil {
				inHunk = false
				continue
			}
			oldLine = os
			newLine = ns
			inHunk = true

		default:
			if !inHunk || cur == nil {
				continue
			}
			if len(line) == 0 {
				// A bare empty line inside a hunk is a context line ("" with the
				// leading space stripped by some tools); advance both.
				cur.NewLines[newLine] = true
				cur.OldLines[oldLine] = true
				newLine++
				oldLine++
				continue
			}
			switch line[0] {
			case '+':
				cur.NewLines[newLine] = true
				newLine++
			case '-':
				cur.OldLines[oldLine] = true
				oldLine++
			case ' ':
				cur.NewLines[newLine] = true
				cur.OldLines[oldLine] = true
				newLine++
				oldLine++
			case '\\':
				// "\ No newline at end of file" — not a content line.
			default:
				// Unknown line; end the hunk to avoid mis-counting.
				inHunk = false
			}
		}
	}

	return dm
}

// stripDiffPath cleans a path token from a "---"/"+++" line: trims a trailing
// tab-separated timestamp and a leading "a/" or "b/" prefix.
func stripDiffPath(s string) string {
	s = strings.TrimSpace(s)
	// Some diffs append "\t<timestamp>" after the path.
	if i := strings.IndexByte(s, '\t'); i >= 0 {
		s = s[:i]
	}
	if s == "/dev/null" {
		return s
	}
	if strings.HasPrefix(s, "a/") || strings.HasPrefix(s, "b/") {
		return s[2:]
	}
	return s
}

// parseHunkHeader parses "@@ -oldStart,oldCount +newStart,newCount @@ ..." and
// returns the starting old and new line numbers.
func parseHunkHeader(line string) (oldStart, newStart int, ok bool) {
	// Find the "@@ ... @@" core.
	rest := strings.TrimPrefix(line, "@@")
	end := strings.Index(rest, "@@")
	if end < 0 {
		return 0, 0, false
	}
	core := strings.TrimSpace(rest[:end])
	fields := strings.Fields(core)
	var gotOld, gotNew bool
	for _, f := range fields {
		switch {
		case strings.HasPrefix(f, "-"):
			oldStart = parseRangeStart(f[1:])
			gotOld = true
		case strings.HasPrefix(f, "+"):
			newStart = parseRangeStart(f[1:])
			gotNew = true
		}
	}
	if !gotOld || !gotNew {
		return 0, 0, false
	}
	return oldStart, newStart, true
}

// parseRangeStart parses "start" or "start,count" and returns start.
func parseRangeStart(s string) int {
	if i := strings.IndexByte(s, ','); i >= 0 {
		s = s[:i]
	}
	n, err := strconv.Atoi(s)
	if err != nil {
		return 0
	}
	return n
}
