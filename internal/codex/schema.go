package codex

import (
	_ "embed"
	"fmt"
	"os"
)

// findingsSchema is the JSON Schema codex must conform to for `--output-schema`.
// codex requires a file path, so callers write this to a temp file at runtime.
//
//go:embed findings.schema.json
var findingsSchema []byte

// writeSchemaTemp materializes the embedded schema to a temp file and returns
// its path plus a cleanup func. codex's --output-schema flag needs a file path.
func writeSchemaTemp() (path string, cleanup func(), err error) {
	f, err := os.CreateTemp("", "codex-schema-*.json")
	if err != nil {
		return "", func() {}, fmt.Errorf("create schema temp file: %w", err)
	}
	name := f.Name()
	if _, err := f.Write(findingsSchema); err != nil {
		f.Close()
		os.Remove(name)
		return "", func() {}, fmt.Errorf("write schema temp file: %w", err)
	}
	if err := f.Close(); err != nil {
		os.Remove(name)
		return "", func() {}, fmt.Errorf("close schema temp file: %w", err)
	}
	return name, func() { os.Remove(name) }, nil
}
