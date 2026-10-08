package chat

import (
	"encoding/json"
	"testing"
)

// TestExtractThinkingText covers the tolerant extraction of a "thinking"
// content block's "thinking" field in both wire shapes.
func TestExtractThinkingText(t *testing.T) {
	tests := []struct {
		name string
		raw  string
		want string
	}{
		{"plain string", `"abc"`, "abc"},
		{"array mixing objects and strings", `[{"text":"a"},"b",{"text":"c"}]`, "abc"},
		{"object without text field", `[{"other":1}]`, ""},
		{"null", `null`, ""},
		{"number", `42`, ""},
		{"non-string text field", `["x",{"text":123}]`, "x"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := extractThinkingText(json.RawMessage(tt.raw))
			if got != tt.want {
				t.Fatalf("extractThinkingText(%s) = %q, want %q", tt.raw, got, tt.want)
			}
		})
	}
}
