package chat

import (
	"context"
	"encoding/json"
	"io"
	"strings"
	"testing"

	"providerbridge/internal/format"
)

// TestDeltaUnmarshalJSON covers tolerant decoding of delta.content in both
// the plain string form and the block-array form (issue #11).
func TestDeltaUnmarshalJSON(t *testing.T) {
	t.Run("plain string content", func(t *testing.T) {
		var d Delta
		if err := json.Unmarshal([]byte(`{"content":"PONG"}`), &d); err != nil {
			t.Fatalf("unmarshal error: %v", err)
		}
		if d.Content != "PONG" {
			t.Fatalf("Content = %q, want %q", d.Content, "PONG")
		}
	})

	t.Run("issue-shaped array content", func(t *testing.T) {
		var d Delta
		raw := `{"role":"assistant","content":[{"type":"thinking","thinking":[{"text":"hmm"}]},{"type":"text","text":"PONG"}]}`
		if err := json.Unmarshal([]byte(raw), &d); err != nil {
			t.Fatalf("unmarshal error: %v", err)
		}
		if d.Role != "assistant" {
			t.Fatalf("Role = %q, want %q", d.Role, "assistant")
		}
		if d.Content != "PONG" {
			t.Fatalf("Content = %q, want %q", d.Content, "PONG")
		}
		if d.ReasoningContent != "hmm" {
			t.Fatalf("ReasoningContent = %q, want %q", d.ReasoningContent, "hmm")
		}
	})

	t.Run("thinking as string", func(t *testing.T) {
		var d Delta
		raw := `{"role":"assistant","content":[{"type":"thinking","thinking":"why"}]}`
		if err := json.Unmarshal([]byte(raw), &d); err != nil {
			t.Fatalf("unmarshal error: %v", err)
		}
		if d.ReasoningContent != "why" {
			t.Fatalf("ReasoningContent = %q, want %q", d.ReasoningContent, "why")
		}
		if d.Content != "" {
			t.Fatalf("Content = %q, want empty", d.Content)
		}
	})

	t.Run("two text blocks concatenate", func(t *testing.T) {
		var d Delta
		raw := `{"content":[{"type":"text","text":"A"},{"type":"text","text":"B"}]}`
		if err := json.Unmarshal([]byte(raw), &d); err != nil {
			t.Fatalf("unmarshal error: %v", err)
		}
		if d.Content != "AB" {
			t.Fatalf("Content = %q, want %q", d.Content, "AB")
		}
	})

	t.Run("role only yields empty fields", func(t *testing.T) {
		var d Delta
		if err := json.Unmarshal([]byte(`{"role":"assistant"}`), &d); err != nil {
			t.Fatalf("unmarshal error: %v", err)
		}
		if d.Role != "assistant" || d.Content != "" || d.ReasoningContent != "" || len(d.ToolCalls) != 0 {
			t.Fatalf("unexpected delta: %+v", d)
		}
	})

	t.Run("tool_calls pass through", func(t *testing.T) {
		var d Delta
		raw := `{"role":"assistant","tool_calls":[{"index":0,"id":"call_1","type":"function","function":{"name":"f","arguments":"{}"}}]}`
		if err := json.Unmarshal([]byte(raw), &d); err != nil {
			t.Fatalf("unmarshal error: %v", err)
		}
		if len(d.ToolCalls) != 1 {
			t.Fatalf("ToolCalls len = %d, want 1", len(d.ToolCalls))
		}
		if d.Content != "" {
			t.Fatalf("Content = %q, want empty", d.Content)
		}
	})

	t.Run("non-string non-array content errors", func(t *testing.T) {
		var d Delta
		if err := json.Unmarshal([]byte(`{"content":true}`), &d); err == nil {
			t.Fatalf("expected error, got none")
		}
	})
}

// TestFromChatContentThinkingBlock proves fromChatContent maps a
// "thinking" block to a reasoning CoreContentBlock (non-stream parity for
// issue #11) while other block types keep their existing behavior.
func TestFromChatContentThinkingBlock(t *testing.T) {
	adapter := NewChatProviderAdapter(0, nil, format.CorePluginHooks{})
	blocks := adapter.fromChatContent([]any{
		map[string]any{
			"type":     "thinking",
			"thinking": []any{map[string]any{"text": "why"}},
		},
		map[string]any{"type": "text", "text": "hi"},
	})
	if len(blocks) != 2 {
		t.Fatalf("got %d blocks, want 2: %+v", len(blocks), blocks)
	}
	if blocks[0].Type != "reasoning" || blocks[0].ReasoningText != "why" {
		t.Fatalf("blocks[0] = %+v, want reasoning block with ReasoningText %q", blocks[0], "why")
	}
	if blocks[1].Type != "text" || blocks[1].Text != "hi" {
		t.Fatalf("blocks[1] = %+v, want text block with Text %q", blocks[1], "hi")
	}
}

// TestToCoreStreamArrayFormDelta proves the wire chunk → Core event
// pipeline emits the reasoning delta before the text delta for array-form
// content chunks (issue #11), with a reasoning ContentBlock carrying the
// reasoning text, and that the stream ends with a completion event.
func TestToCoreStreamArrayFormDelta(t *testing.T) {
	raw := `{"id":"1","object":"chat.completion.chunk","created":1,"model":"test-model","choices":[{"index":0,"delta":{"role":"assistant","content":[{"type":"thinking","thinking":[{"text":"hmm"}]},{"type":"text","text":"PONG"}]}}]}`
	var chunk ChatStreamChunk
	if err := json.Unmarshal([]byte(raw), &chunk); err != nil {
		t.Fatalf("unmarshal error: %v", err)
	}
	src := make(chan ChatStreamChunk, 1)
	src <- chunk
	close(src)

	adapter := NewChatProviderAdapter(0, nil, format.CorePluginHooks{})
	sr, err := adapter.ToCoreStream(context.Background(), (<-chan ChatStreamChunk)(src))
	if err != nil {
		t.Fatalf("ToCoreStream error: %v", err)
	}
	var events []format.CoreStreamEvent
	for ev := range sr.Events {
		events = append(events, ev)
	}
	if len(events) == 0 {
		t.Fatal("no events collected")
	}

	reasoningIdx, textIdx := -1, -1
	for i, ev := range events {
		if ev.Type != format.CoreTextDelta {
			continue
		}
		if ev.Delta == "hmm" && reasoningIdx == -1 {
			reasoningIdx = i
		}
		if ev.Delta == "PONG" {
			textIdx = i
		}
	}
	if reasoningIdx == -1 {
		t.Fatalf("no reasoning text delta %q found", "hmm")
	}
	if textIdx == -1 {
		t.Fatalf("no text delta %q found", "PONG")
	}
	if reasoningIdx >= textIdx {
		t.Fatalf("reasoning delta (event %d) must precede text delta (event %d)", reasoningIdx, textIdx)
	}

	var haveReasoningBlock bool
	for i, ev := range events {
		if ev.ContentBlock != nil && ev.ContentBlock.Type == "reasoning" && ev.ContentBlock.ReasoningText == "hmm" {
			if i >= textIdx {
				t.Fatalf("reasoning ContentBlock at event %d must precede text delta at event %d", i, textIdx)
			}
			haveReasoningBlock = true
		}
	}
	if !haveReasoningBlock {
		t.Fatalf("no reasoning ContentBlock with ReasoningText %q found", "hmm")
	}

	if last := events[len(events)-1]; last.Type != format.CoreEventCompleted {
		t.Fatalf("last event = %v, want %v", last.Type, format.CoreEventCompleted)
	}
}

// TestReadStreamToleratesArrayDelta proves the SSE reader keeps the
// issue-shaped array-form delta chunk (issue #11) and that a garbage data
// line drops without killing the stream.
func TestReadStreamToleratesArrayDelta(t *testing.T) {
	issueChunk := `{"id":"1","object":"chat.completion.chunk","created":1,"model":"test-model","choices":[{"index":0,"delta":{"role":"assistant","content":[{"type":"thinking","thinking":[{"text":"hmm"}]},{"type":"text","text":"PONG"}]}}]}`
	tailChunk := `{"id":"2","object":"chat.completion.chunk","created":1,"model":"test-model","choices":[{"index":0,"delta":{"content":"TAIL"}}]}`
	lines := "data: " + issueChunk + "\n" +
		"data: not-json-garbage\n" +
		"data: " + tailChunk + "\n" +
		"data: " + "[" + "DONE" + "]" + "\n"

	c := NewClient(ClientConfig{BaseURL: "http://unused.test"})
	ch := make(chan ChatStreamChunk, 8)
	c.readStream(context.Background(), io.NopCloser(strings.NewReader(lines)), ch)

	var got []ChatStreamChunk
	for chunk := range ch {
		got = append(got, chunk)
	}
	if len(got) != 2 {
		t.Fatalf("got %d chunks, want 2: %+v", len(got), got)
	}
	d0 := got[0].Choices[0].Delta
	if d0.Content != "PONG" || d0.ReasoningContent != "hmm" {
		t.Fatalf("chunk0 delta = %+v, want Content %q and ReasoningContent %q", d0, "PONG", "hmm")
	}
	d1 := got[1].Choices[0].Delta
	if d1.Content != "TAIL" {
		t.Fatalf("chunk1 delta = %+v, want Content %q", d1, "TAIL")
	}
}
