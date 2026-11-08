// Package chat_test: streaming sequence tests for the Chat provider adapter.
//
// Covers the premature-text→reasoning transition seen on Mistral GLM
// (role=assistant first, reasoning_content in later chunks): the Core
// sequence must be start(text,0) → done(0) → start(reasoning,1) → …
package chat_test

import (
	"context"
	"testing"

	"providerbridge/internal/format"
	"providerbridge/internal/protocol/chat"
)

// TestToCoreStream_PrematureTextThenReasoning asserts the corrected Core
// event sequence when a provider sends role=assistant (starting a text
// block) and only later streams reasoning_content: the dangling text block
// must be closed at its own index BEFORE the reasoning block starts.
func TestToCoreStream_PrematureTextThenReasoning(t *testing.T) {
	adapter := chat.NewChatProviderAdapter(0, nil, format.CorePluginHooks{})
	src := make(chan chat.ChatStreamChunk, 8)
	// chunk1: role only — starts a text block at index 0.
	src <- chat.ChatStreamChunk{
		ID:      "chunk1",
		Choices: []chat.StreamChoice{{Index: 0, Delta: chat.Delta{Role: "assistant"}}},
	}
	// chunk2-3: reasoning_content arrives after the text block started.
	src <- chat.ChatStreamChunk{
		Choices: []chat.StreamChoice{{Index: 0, Delta: chat.Delta{ReasoningContent: "Let me think"}}},
	}
	src <- chat.ChatStreamChunk{
		Choices: []chat.StreamChoice{{Index: 0, Delta: chat.Delta{ReasoningContent: " more"}}},
	}
	// chunk4-5: content after reasoning — reasoning closes, text block 2 opens.
	src <- chat.ChatStreamChunk{
		Choices: []chat.StreamChoice{{Index: 0, Delta: chat.Delta{Content: "Hello"}}},
	}
	src <- chat.ChatStreamChunk{
		Choices: []chat.StreamChoice{{Index: 0, Delta: chat.Delta{Content: " world"}}},
	}
	// chunk6: finish.
	src <- chat.ChatStreamChunk{
		Choices: []chat.StreamChoice{{Index: 0, Delta: chat.Delta{}, FinishReason: "stop"}},
	}
	src <- chat.ChatStreamChunk{
		Usage: &chat.Usage{PromptTokens: 5, CompletionTokens: 10, TotalTokens: 15},
	}
	close(src)

	events, err := adapter.ToCoreStream(context.Background(), (<-chan chat.ChatStreamChunk)(src))
	if err != nil {
		t.Fatal(err)
	}

	var evts []format.CoreStreamEvent
	for e := range events.Events {
		evts = append(evts, e)
	}

	type want struct {
		Type  format.CoreStreamEventType
		Index int
	}
	wantSeq := []want{
		{format.CoreContentBlockStarted, 0}, // text block starts (role chunk)
		{format.CoreContentBlockDone, 0},    // premature text block closed at ITS index
		{format.CoreContentBlockStarted, 1}, // reasoning block starts
		{format.CoreTextDelta, 1},           // reasoning deltas
		{format.CoreTextDelta, 1},
		{format.CoreContentBlockDone, 1},    // reasoning closed
		{format.CoreContentBlockStarted, 2}, // text block 2 starts
		{format.CoreTextDelta, 2},
		{format.CoreTextDelta, 2},
		{format.CoreContentBlockDone, 2}, // finish closes text block 2
		{format.CoreEventCompleted, 0},
	}

	if len(evts) != len(wantSeq) {
		for i, e := range evts {
			t.Logf("evts[%d] = %s index=%d delta=%q", i, e.Type, e.Index, e.Delta)
		}
		t.Fatalf("got %d events, want %d", len(evts), len(wantSeq))
	}
	for i, w := range wantSeq {
		if evts[i].Type != w.Type || evts[i].Index != w.Index {
			t.Fatalf("evts[%d] = %s index=%d, want %s index=%d", i, evts[i].Type, evts[i].Index, w.Type, w.Index)
		}
	}
	// The reasoning block start must be a reasoning block; the text blocks text.
	if evts[2].ContentBlock == nil || evts[2].ContentBlock.Type != "reasoning" {
		t.Errorf("evts[2].ContentBlock = %+v, want reasoning block", evts[2].ContentBlock)
	}
	if evts[6].ContentBlock == nil || evts[6].ContentBlock.Type != "text" {
		t.Errorf("evts[6].ContentBlock = %+v, want text block", evts[6].ContentBlock)
	}
}

// TestToCoreResponseWithRequest_ModelFromChatResponse asserts the Core
// response carries the upstream model name (required by the Anthropic
// inbound to populate the Message.model field).
func TestToCoreResponseWithRequest_ModelFromChatResponse(t *testing.T) {
	adapter := chat.NewChatProviderAdapter(0, nil, format.CorePluginHooks{})
	chatResp := &chat.ChatResponse{
		ID:    "resp_1",
		Model: "mistral/zai-glm-5-3",
		Choices: []chat.Choice{{
			Index:        0,
			Message:      chat.ChatMessage{Role: "assistant", Content: "hi"},
			FinishReason: "stop",
		}},
	}
	coreResp, err := adapter.ToCoreResponseWithRequest(context.Background(), nil, chatResp)
	if err != nil {
		t.Fatal(err)
	}
	if coreResp.Model != "mistral/zai-glm-5-3" {
		t.Errorf("coreResp.Model = %q, want mistral/zai-glm-5-3", coreResp.Model)
	}
}

// TestToCoreResponseWithRequest_ModelFallsBackToRequest asserts that when
// the upstream response omits the model, the request's model is used.
func TestToCoreResponseWithRequest_ModelFallsBackToRequest(t *testing.T) {
	adapter := chat.NewChatProviderAdapter(0, nil, format.CorePluginHooks{})
	chatResp := &chat.ChatResponse{
		ID: "resp_2",
		Choices: []chat.Choice{{
			Index:        0,
			Message:      chat.ChatMessage{Role: "assistant", Content: "hi"},
			FinishReason: "stop",
		}},
	}
	req := &format.CoreRequest{Model: "req-model"}
	coreResp, err := adapter.ToCoreResponseWithRequest(context.Background(), req, chatResp)
	if err != nil {
		t.Fatal(err)
	}
	if coreResp.Model != "req-model" {
		t.Errorf("coreResp.Model = %q, want req-model", coreResp.Model)
	}
}
