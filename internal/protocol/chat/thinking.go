// Package chat implements the OpenAI Chat Completions ProviderAdapter for ProviderBridge.

package chat

import (
	"encoding/json"
	"strings"
)

// extractThinkingText extracts the text of a "thinking" content block's
// "thinking" field, tolerating both wire shapes: a plain string
// ("thinking":"…") and an array ("thinking":[{"text":"…"},"bare"]).
// String elements and "text" fields of object elements concatenate in wire
// order. Any other shape yields "" (never an error).
func extractThinkingText(raw json.RawMessage) string {
	if len(raw) == 0 || string(raw) == "null" {
		return ""
	}
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		return s
	}
	var arr []json.RawMessage
	if err := json.Unmarshal(raw, &arr); err != nil {
		return ""
	}
	var sb strings.Builder
	for _, el := range arr {
		var es string
		if err := json.Unmarshal(el, &es); err == nil {
			sb.WriteString(es)
			continue
		}
		var obj map[string]json.RawMessage
		if err := json.Unmarshal(el, &obj); err == nil {
			if t, ok := obj["text"]; ok {
				var ts string
				if json.Unmarshal(t, &ts) == nil {
					sb.WriteString(ts)
				}
			}
		}
	}
	return sb.String()
}
