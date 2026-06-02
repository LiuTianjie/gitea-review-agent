package codex

import (
	"encoding/json"
	"testing"
)

func TestFindingsSchemaRequiresEveryTopLevelProperty(t *testing.T) {
	var schema struct {
		Required   []string       `json:"required"`
		Properties map[string]any `json:"properties"`
	}
	if err := json.Unmarshal(findingsSchema, &schema); err != nil {
		t.Fatalf("schema JSON invalid: %v", err)
	}
	required := map[string]bool{}
	for _, key := range schema.Required {
		required[key] = true
	}
	for key := range schema.Properties {
		if !required[key] {
			t.Fatalf("schema property %q is missing from required", key)
		}
	}
}
