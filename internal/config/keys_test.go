package config

import (
	"reflect"
	"testing"
)

// TestSplitAPIKeys verifies comma-separated api_key parsing: trimming,
// empty-segment dropping, and nil for values that yield no keys.
func TestSplitAPIKeys(t *testing.T) {
	tests := []struct {
		name string
		raw  string
		want []string
	}{
		{"empty", "", nil},
		{"only commas", ",", nil},
		{"single", "a", []string{"a"}},
		{"trimmed", " a , b ", []string{"a", "b"}},
		{"empty middle", "a,,b", []string{"a", "b"}},
		{"whitespace segment", "a, ,b", []string{"a", "b"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := SplitAPIKeys(tt.raw)
			if tt.want == nil {
				if got != nil {
					t.Errorf("SplitAPIKeys(%q) = %v, want nil", tt.raw, got)
				}
				return
			}
			if !reflect.DeepEqual(got, tt.want) {
				t.Errorf("SplitAPIKeys(%q) = %v, want %v", tt.raw, got, tt.want)
			}
		})
	}
}
