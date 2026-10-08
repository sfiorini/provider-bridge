// Package config defines ProviderBridge configuration types and YAML loading.

package config

import "strings"

// SplitAPIKeys splits a comma-separated api_key value into individual API
// keys. Whitespace around each segment is trimmed and empty segments are
// dropped. A value with no commas yields a single-element slice. A value
// that yields no keys (empty, or only commas/whitespace) returns nil —
// callers treat nil as an invalid provider.
//
// APIKey remains the single canonical string everywhere in config/store/
// graph; parsing happens only at runtime-client build time.
func SplitAPIKeys(raw string) []string {
	if raw == "" {
		return nil
	}
	parts := strings.Split(raw, ",")
	keys := make([]string, 0, len(parts))
	for _, p := range parts {
		if k := strings.TrimSpace(p); k != "" {
			keys = append(keys, k)
		}
	}
	if len(keys) == 0 {
		return nil
	}
	return keys
}
