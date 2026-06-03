package claude

import (
	"encoding/json"
	"testing"
)

func TestFindingsSchemaRequiresEveryProperty(t *testing.T) {
	var schema struct {
		Required   []string       `json:"required"`
		Properties map[string]any `json:"properties"`
	}
	if err := json.Unmarshal(findingsSchema, &schema); err != nil {
		t.Fatalf("schema JSON invalid: %v", err)
	}
	assertRequiredCoversProperties(t, "top-level", schema.Required, schema.Properties)

	findings, ok := schema.Properties["findings"].(map[string]any)
	if !ok {
		t.Fatalf("findings property missing or invalid")
	}
	items, ok := findings["items"].(map[string]any)
	if !ok {
		t.Fatalf("findings.items missing or invalid")
	}
	itemRequired, ok := items["required"].([]any)
	if !ok {
		t.Fatalf("findings.items.required missing or invalid")
	}
	itemProperties, ok := items["properties"].(map[string]any)
	if !ok {
		t.Fatalf("findings.items.properties missing or invalid")
	}
	assertRequiredCoversProperties(t, "findings.items", stringsFromAny(itemRequired), itemProperties)
}

func assertRequiredCoversProperties(t *testing.T, label string, requiredList []string, properties map[string]any) {
	t.Helper()
	required := map[string]bool{}
	for _, key := range requiredList {
		required[key] = true
	}
	for key := range properties {
		if !required[key] {
			t.Fatalf("%s schema property %q is missing from required", label, key)
		}
	}
}

func stringsFromAny(values []any) []string {
	out := make([]string, 0, len(values))
	for _, value := range values {
		if s, ok := value.(string); ok {
			out = append(out, s)
		}
	}
	return out
}
