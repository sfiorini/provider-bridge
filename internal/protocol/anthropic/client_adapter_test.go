package anthropic

import (
	"context"
	"encoding/json"
	"strings"
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

// ============================================================================
// Strict-SDK wire shape (Claude Code / @anthropic-ai/sdk zod validation)
// ============================================================================

// TestStreamEventMarshal_ContentBlockEventsAlwaysCarryIndex asserts that
// content_block_start/stop/delta SSE events always carry "index" — including
// index 0 — while non-content_block events (message_stop, ping) carry none.
func TestStreamEventMarshal_ContentBlockEventsAlwaysCarryIndex(t *testing.T) {
	start := StreamEvent{Type: "content_block_start", Index: 0, ContentBlock: &ContentBlock{Type: "text"}}
	data, err := json.Marshal(start)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), `"index":0`) {
		t.Errorf("content_block_start at index 0 missing \"index\":0: %s", data)
	}

	stop := StreamEvent{Type: "content_block_stop", Index: 0}
	data, err = json.Marshal(stop)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), `"index":0`) {
		t.Errorf("content_block_stop at index 0 missing \"index\":0: %s", data)
	}

	delta := StreamEvent{Type: "content_block_delta", Index: 0, Delta: StreamDelta{Type: "text_delta", Text: "hi"}}
	data, err = json.Marshal(delta)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), `"index":0`) {
		t.Errorf("content_block_delta at index 0 missing \"index\":0: %s", data)
	}

	for _, ev := range []StreamEvent{
		{Type: "message_stop"},
		{Type: "ping"},
		{Type: "message_start", Message: &MessageResponse{ID: "msg_1", Type: "message", Role: "assistant", Model: "m"}},
	} {
		data, err := json.Marshal(ev)
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(string(data), `"index"`) {
			t.Errorf("%s event must not carry an index field: %s", ev.Type, data)
		}
	}
}

// TestContentBlockMarshal_TextAlwaysEmitsTextField asserts a text
// content_block_start carries "text":"" (Anthropic emits {"type":"text","text":""}).
func TestContentBlockMarshal_TextAlwaysEmitsTextField(t *testing.T) {
	data, err := json.Marshal(ContentBlock{Type: "text"})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), `"text":""`) {
		t.Errorf("text content block must always emit the text field: %s", data)
	}
	// Thinking blocks are unchanged.
	data, err = json.Marshal(ContentBlock{Type: "thinking", Thinking: "hmm", Signature: "sig"})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), `"thinking":"hmm"`) || strings.Contains(string(data), `"text"`) {
		t.Errorf("thinking block shape changed: %s", data)
	}
}

// TestMessageResponseMarshal_RequiredFields asserts the Message schema the
// strict SDK validates: id, type, role, content, model, stop_reason,
// stop_sequence, usage are always present.
func TestMessageResponseMarshal_RequiredFields(t *testing.T) {
	resp := MessageResponse{
		ID: "msg_1", Type: "message", Role: "assistant", Model: "mistral/zai-glm-5-3",
		Content: []ContentBlock{}, StopReason: "end_turn",
	}
	data, err := json.Marshal(resp)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		`"model":"mistral/zai-glm-5-3"`,
		`"stop_reason":"end_turn"`,
		`"stop_sequence":""`,
		`"content":[]`,
		`"usage"`,
	} {
		if !strings.Contains(string(data), want) {
			t.Errorf("MessageResponse missing required field %s: %s", want, data)
		}
	}
}

// TestStreamDeltaMarshal_MessageDeltaCarriesBothStopFields asserts
// message_delta deltas always carry both stop_reason and stop_sequence,
// and content deltas keep their existing shape.
func TestStreamDeltaMarshal_MessageDeltaCarriesBothStopFields(t *testing.T) {
	data, err := json.Marshal(StreamDelta{StopReason: "end_turn"})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), `"stop_reason":"end_turn"`) {
		t.Errorf("message_delta missing stop_reason: %s", data)
	}
	if !strings.Contains(string(data), `"stop_sequence":""`) {
		t.Errorf("message_delta missing stop_sequence: %s", data)
	}

	// Content delta shapes unchanged.
	cases := []struct {
		delta StreamDelta
		want  string
	}{
		{StreamDelta{Type: "text_delta", Text: "hi"}, `"type":"text_delta","text":"hi"`},
		{StreamDelta{Type: "thinking_delta", Thinking: "hmm"}, `"type":"thinking_delta","thinking":"hmm"`},
		{StreamDelta{Type: "input_json_delta", PartialJSON: `{"a":1}`}, `"type":"input_json_delta","partial_json":"{\"a\":1}"`},
		{StreamDelta{Type: "signature_delta", Signature: "sig"}, `"type":"signature_delta","signature":"sig"`},
	}
	for _, c := range cases {
		data, err := json.Marshal(c.delta)
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(string(data), c.want) {
			t.Errorf("delta %+v shape changed: %s", c.delta, data)
		}
		if strings.Contains(string(data), "stop_reason") {
			t.Errorf("content delta must not carry stop_reason: %s", data)
		}
	}
}

// TestFromCoreStream_WireShapeForStrictSDK runs a canned Core stream through
// the inbound adapter and asserts the exact SSE JSON fields the strict SDK
// requires (message_start message fields, index on content_block events,
// message_delta stop fields, no index on message_stop).
func TestFromCoreStream_WireShapeForStrictSDK(t *testing.T) {
	adapter := NewAnthropicClientAdapter(format.CorePluginHooks{}.WithDefaults())
	ci := 0
	events := make(chan format.CoreStreamEvent, 8)
	events <- format.CoreStreamEvent{Type: format.CoreEventCreated, Model: "mistral/zai-glm-5-3"}
	events <- format.CoreStreamEvent{
		Type: format.CoreContentBlockStarted, Index: 0, ChoiceIndex: &ci,
		ContentBlock: &format.CoreContentBlock{Type: "text"},
	}
	events <- format.CoreStreamEvent{Type: format.CoreTextDelta, Index: 0, ChoiceIndex: &ci, Delta: "Hello"}
	events <- format.CoreStreamEvent{Type: format.CoreContentBlockDone, Index: 0, ChoiceIndex: &ci, StopReason: "end_turn"}
	events <- format.CoreStreamEvent{
		Type: format.CoreEventCompleted, Status: "completed", ChoiceIndex: &ci,
		Usage: &format.CoreUsage{InputTokens: 5, OutputTokens: 7},
	}
	close(events)

	coreReq := &format.CoreRequest{Model: "mistral/zai-glm-5-3"}
	streamAny, err := adapter.FromCoreStream(context.Background(), coreReq, events)
	if err != nil {
		t.Fatalf("FromCoreStream: %v", err)
	}
	result, ok := streamAny.(*AnthropicStreamResult)
	if !ok {
		t.Fatalf("unexpected stream result type %T", streamAny)
	}

	var wire []string
	for ev := range result.Chan() {
		data, err := json.Marshal(ev)
		if err != nil {
			t.Fatal(err)
		}
		wire = append(wire, string(data))
	}

	if len(wire) < 6 {
		t.Fatalf("got %d SSE events, want at least 6: %v", len(wire), wire)
	}
	// message_start: message must carry model, content:[], stop_reason, stop_sequence.
	msgStart := wire[0]
	for _, want := range []string{
		`"type":"message_start"`,
		`"model":"mistral/zai-glm-5-3"`,
		`"content":[]`,
		`"stop_reason"`,
		`"stop_sequence"`,
	} {
		if !strings.Contains(msgStart, want) {
			t.Errorf("message_start missing %s: %s", want, msgStart)
		}
	}
	// content_block_start at index 0 with text:"".
	if !strings.Contains(wire[1], `"index":0`) || !strings.Contains(wire[1], `"text":""`) {
		t.Errorf("content_block_start wire shape wrong: %s", wire[1])
	}
	// content_block_delta carries index.
	if !strings.Contains(wire[2], `"index":0`) || !strings.Contains(wire[2], `"text":"Hello"`) {
		t.Errorf("content_block_delta wire shape wrong: %s", wire[2])
	}
	// content_block_stop carries index.
	if !strings.Contains(wire[3], `"content_block_stop"`) || !strings.Contains(wire[3], `"index":0`) {
		t.Errorf("content_block_stop wire shape wrong: %s", wire[3])
	}
	// message_delta carries both stop fields.
	msgDelta := wire[4]
	if !strings.Contains(msgDelta, `"stop_reason"`) || !strings.Contains(msgDelta, `"stop_sequence"`) {
		t.Errorf("message_delta missing stop fields: %s", msgDelta)
	}
	// message_stop carries no index.
	msgStop := wire[5]
	if !strings.Contains(msgStop, `"message_stop"`) || strings.Contains(msgStop, `"index"`) {
		t.Errorf("message_stop wire shape wrong: %s", msgStop)
	}
}

// TestFromCoreResponse_RequiredFields asserts the non-streaming inbound
// response JSON carries the fields the strict SDK Message schema requires.
func TestFromCoreResponse_RequiredFields(t *testing.T) {
	adapter := NewAnthropicClientAdapter(format.CorePluginHooks{}.WithDefaults())
	coreResp := &format.CoreResponse{
		ID:     "resp_1",
		Status: "completed",
		Model:  "mistral/zai-glm-5-3",
		Messages: []format.CoreMessage{{
			Role:    "assistant",
			Content: []format.CoreContentBlock{{Type: "text", Text: "Hello"}},
		}},
		StopReason: "end_turn",
	}
	out, err := adapter.FromCoreResponse(context.Background(), coreResp)
	if err != nil {
		t.Fatalf("FromCoreResponse: %v", err)
	}
	data, err := json.Marshal(out)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		`"model":"mistral/zai-glm-5-3"`,
		`"stop_reason":"end_turn"`,
		`"stop_sequence":""`,
		`"content":[{"type":"text","text":"Hello"}]`,
	} {
		if !strings.Contains(string(data), want) {
			t.Errorf("response missing %s: %s", want, data)
		}
	}
}
