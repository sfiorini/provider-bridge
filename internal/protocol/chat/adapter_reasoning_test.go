// Reasoning-echo gating: providers other than DeepSeek (notably Mistral)
// reject reasoning_content on input assistant messages with 422
// extra_forbidden, so FromCoreRequest must strip it unless the provider key
// says the upstream requires the echo (DeepSeek).
package chat

import (
	"encoding/json"
	"strings"
	"testing"

	"providerbridge/internal/format"
)

func reasoningEchoCoreRequest() *format.CoreRequest {
	return &format.CoreRequest{
		Model: "upstream-model",
		Messages: []format.CoreMessage{
			{Role: "user", Content: []format.CoreContentBlock{{Type: "text", Text: "hi"}}},
			{Role: "assistant", Content: []format.CoreContentBlock{
				{Type: "reasoning", ReasoningText: "secret thoughts"},
				{Type: "text", Text: "answer"},
				{Type: "tool_use", ToolUseID: "tu1", ToolName: "get_weather", ToolInput: json.RawMessage(`{"city":"Rome"}`)},
			}},
		},
	}
}

func findAssistantMessage(t *testing.T, chatReq *ChatRequest) ChatMessage {
	t.Helper()
	for _, m := range chatReq.Messages {
		if m.Role == "assistant" {
			return m
		}
	}
	t.Fatalf("no assistant message in built ChatRequest")
	return ChatMessage{}
}

func TestFromCoreRequest_ReasoningEcho_StrippedForNonDeepSeekProvider(t *testing.T) {
	adapter := NewChatProviderAdapter(0, nil, format.CorePluginHooks{})
	core := reasoningEchoCoreRequest()
	core.Extensions = map[string]any{"provider_key": "mistral"}

	upstream, err := adapter.FromCoreRequest(t.Context(), core)
	if err != nil {
		t.Fatalf("FromCoreRequest: %v", err)
	}
	chatReq, ok := upstream.(*ChatRequest)
	if !ok {
		t.Fatalf("upstream type = %T, want *ChatRequest", upstream)
	}

	data, err := json.Marshal(chatReq)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	if strings.Contains(string(data), "reasoning_content") {
		t.Errorf("body must not contain reasoning_content for mistral: %s", data)
	}

	asst := findAssistantMessage(t, chatReq)
	if asst.ReasoningContent != "" {
		t.Errorf("assistant ReasoningContent = %q, want empty", asst.ReasoningContent)
	}
	if asst.EmitEmptyReasoningContent {
		t.Error("assistant EmitEmptyReasoningContent = true, want false")
	}
	// The text and tool_calls must survive the strip.
	if s, _ := asst.Content.(string); s != "answer" {
		t.Errorf("assistant content = %v, want %q", asst.Content, "answer")
	}
	if len(asst.ToolCalls) != 1 || asst.ToolCalls[0].ID != "tu1" {
		t.Errorf("assistant ToolCalls = %+v, want one call with ID tu1", asst.ToolCalls)
	}
}

func TestFromCoreRequest_ReasoningEcho_KeptForDeepSeek(t *testing.T) {
	adapter := NewChatProviderAdapter(0, nil, format.CorePluginHooks{})
	core := reasoningEchoCoreRequest()
	core.Extensions = map[string]any{"provider_key": "deepseek"}

	upstream, err := adapter.FromCoreRequest(t.Context(), core)
	if err != nil {
		t.Fatalf("FromCoreRequest: %v", err)
	}
	chatReq := upstream.(*ChatRequest)
	asst := findAssistantMessage(t, chatReq)
	if asst.ReasoningContent != "secret thoughts" {
		t.Errorf("assistant ReasoningContent = %q, want %q", asst.ReasoningContent, "secret thoughts")
	}
}

func TestFromCoreRequest_ReasoningEcho_DefaultKeptWithoutExtension(t *testing.T) {
	adapter := NewChatProviderAdapter(0, nil, format.CorePluginHooks{})
	core := reasoningEchoCoreRequest()

	upstream, err := adapter.FromCoreRequest(t.Context(), core)
	if err != nil {
		t.Fatalf("FromCoreRequest: %v", err)
	}
	chatReq := upstream.(*ChatRequest)
	asst := findAssistantMessage(t, chatReq)
	if asst.ReasoningContent != "secret thoughts" {
		t.Errorf("assistant ReasoningContent = %q, want %q (absent provider key keeps current behavior)", asst.ReasoningContent, "secret thoughts")
	}
}

func TestStripAssistantReasoningEcho(t *testing.T) {
	msgs := []ChatMessage{
		{Role: "user", Content: "hi"},
		{Role: "assistant", ReasoningContent: "x", EmitEmptyReasoningContent: true},
	}
	stripAssistantReasoningEcho(msgs)
	if msgs[1].ReasoningContent != "" {
		t.Errorf("ReasoningContent = %q, want empty", msgs[1].ReasoningContent)
	}
	if msgs[1].EmitEmptyReasoningContent {
		t.Error("EmitEmptyReasoningContent = true, want false")
	}
	// Non-assistant messages are untouched.
	if msgs[0].Role != "user" || msgs[0].Content != "hi" {
		t.Errorf("user message modified: %+v", msgs[0])
	}
}
