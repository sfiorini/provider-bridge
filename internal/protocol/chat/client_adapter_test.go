package chat

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"moonbridge/internal/format"
)

// TestInboundChatStreamToolCalls feeds a canned Core event sequence
// (tool_use block with argument deltas) through FromCoreStream and asserts
// the emitted Chat Completions chunks.
func TestInboundChatStreamToolCalls(t *testing.T) {
	adapter := NewChatClientAdapter(format.CorePluginHooks{}.WithDefaults())

	ci := 0
	events := make(chan format.CoreStreamEvent, 16)
	events <- format.CoreStreamEvent{Type: format.CoreEventCreated, Model: "test-model"}
	events <- format.CoreStreamEvent{
		Type: format.CoreContentBlockStarted, Index: 0, ChoiceIndex: &ci,
		ContentBlock: &format.CoreContentBlock{Type: "tool_use", ToolUseID: "call_01", ToolName: "get_weather"},
	}
	events <- format.CoreStreamEvent{
		Type: format.CoreToolCallArgsDelta, Index: 0, ChoiceIndex: &ci,
		Delta: `{"city": "Rome"}`,
	}
	events <- format.CoreStreamEvent{Type: format.CoreContentBlockDone, Index: 0, ChoiceIndex: &ci, StopReason: "tool_use"}
	events <- format.CoreStreamEvent{
		Type: format.CoreEventCompleted, Status: "completed", ChoiceIndex: &ci,
		Usage: &format.CoreUsage{InputTokens: 10, OutputTokens: 5},
	}
	close(events)

	coreReq := &format.CoreRequest{Model: "test-model"}

	streamAny, err := adapter.FromCoreStream(context.Background(), coreReq, events)
	if err != nil {
		t.Fatalf("FromCoreStream: %v", err)
	}
	result, ok := streamAny.(*ChatClientStreamResult)
	if !ok {
		t.Fatalf("unexpected stream result type %T", streamAny)
	}

	var chunks []ChatStreamChunk
	deadline := time.After(5 * time.Second)
collect:
	for {
		select {
		case chunk, more := <-result.Chan():
			if !more {
				break collect
			}
			chunks = append(chunks, chunk)
		case <-deadline:
			t.Fatal("timed out waiting for stream chunks")
		}
	}

	wire, _ := json.MarshalIndent(chunks, "", "  ")
	t.Logf("chunks:\n%s", wire)

	if len(chunks) != 3 {
		t.Fatalf("expected 3 chunks (tool call start + args + final), got %d", len(chunks))
	}
	first := chunks[0].Choices[0].Delta
	if len(first.ToolCalls) != 1 || first.ToolCalls[0].Function.Name != "get_weather" || first.ToolCalls[0].ID != "call_01" {
		t.Fatalf("first chunk missing tool call: %+v", first)
	}
	if first.ToolCalls[0].Function.Arguments == nil || string(first.ToolCalls[0].Function.Arguments) != `""` {
		t.Fatalf("first chunk arguments = %q, want empty JSON string", string(first.ToolCalls[0].Function.Arguments))
	}
	second := chunks[1].Choices[0].Delta
	// Arguments are JSON strings on the OpenAI wire ({"city": "Rome"} quoted).
	wantArgs, _ := json.Marshal(`{"city": "Rome"}`)
	if len(second.ToolCalls) != 1 || string(second.ToolCalls[0].Function.Arguments) != string(wantArgs) {
		t.Fatalf("args chunk arguments = %q, want %q", string(second.ToolCalls[0].Function.Arguments), string(wantArgs))
	}
	last := chunks[len(chunks)-1].Choices[0]
	if last.FinishReason != "tool_calls" {
		t.Fatalf("final chunk finish_reason = %q, want tool_calls", last.FinishReason)
	}
}
