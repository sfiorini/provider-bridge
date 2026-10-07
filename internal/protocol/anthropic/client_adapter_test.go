package anthropic

import (
	"context"
	"encoding/json"
	"testing"

	"providerbridge/internal/format"
	"providerbridge/internal/protocol/chat"
)

// TestInboundToolHistoryToChatWire reproduces the Anthropic tool-use history
// conversion through Core into the upstream chat wire format and asserts the
// message order Mistral requires (assistant tool_calls followed by exactly
// one tool message per call).
func TestInboundToolHistoryToChatWire(t *testing.T) {
	inbound := `{
		"model": "mistral/codestral-latest",
		"max_tokens": 300,
		"tools": [{"name": "get_weather", "description": "Get weather", "input_schema": {"type": "object", "properties": {"city": {"type": "string"}}}}],
		"messages": [
			{"role": "user", "content": "Weather in Rome? Use the tool."},
			{"role": "assistant", "content": [{"type": "tool_use", "id": "tu_01", "name": "get_weather", "input": {"city": "Rome"}}]},
			{"role": "user", "content": [{"type": "tool_result", "tool_use_id": "tu_01", "content": "22C sunny"}]}
		]
	}`

	req, err := ParseMessageRequest([]byte(inbound))
	if err != nil {
		t.Fatalf("ParseMessageRequest: %v", err)
	}

	client := NewAnthropicClientAdapter(format.CorePluginHooks{}.WithDefaults())
	coreReq, err := client.ToCoreRequest(context.Background(), req)
	if err != nil {
		t.Fatalf("ToCoreRequest: %v", err)
	}

	t.Logf("core messages:")
	for i, m := range coreReq.Messages {
		types := make([]string, 0, len(m.Content))
		for _, b := range m.Content {
			types = append(types, b.Type+"("+b.ToolUseID+")")
		}
		t.Logf("  [%d] role=%s blocks=%v", i, m.Role, types)
	}
	if coreReq.Messages[2].Role != "tool" {
		t.Fatalf("expected tool_result message to map to Core role tool, got %q", coreReq.Messages[2].Role)
	}

	chatAdapter := chat.NewChatProviderAdapter(0, nil, format.CorePluginHooks{}.WithDefaults())
	chatReqAny, err := chatAdapter.FromCoreRequest(context.Background(), coreReq)
	if err != nil {
		t.Fatalf("FromCoreRequest: %v", err)
	}
	chatReq := chatReqAny.(*chat.ChatRequest)

	wire, _ := json.MarshalIndent(chatReq.Messages, "", "  ")
	t.Logf("chat wire messages:\n%s", wire)

	if len(chatReq.Messages) != 3 {
		t.Fatalf("expected 3 chat messages, got %d", len(chatReq.Messages))
	}
	if chatReq.Messages[2].Role != "tool" || chatReq.Messages[2].ToolCallID != "tu_01" {
		t.Fatalf("expected role=tool with tool_call_id tu_01, got role=%q tool_call_id=%q", chatReq.Messages[2].Role, chatReq.Messages[2].ToolCallID)
	}
}
