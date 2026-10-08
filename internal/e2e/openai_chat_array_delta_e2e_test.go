//go:build e2e

// Package e2e_test contains end-to-end tests that wire the full adapter
// pipeline (inbound client adapter → core → provider adapter) against
// httptest mock upstream servers.
package e2e_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"providerbridge/internal/format"
	"providerbridge/internal/protocol/chat"
	"providerbridge/internal/protocol/openai"
)

// ============================================================================
// TestChatArrayDeltaStreamingE2E
// ============================================================================

// TestChatArrayDeltaStreamingE2E streams a chat completion through the full
// inbound pipeline against a mock upstream that emits the issue-shaped
// array-form delta chunk (issue #11), a finish chunk, and the SSE DONE
// marker. It proves the array-form chunk survives the wire end to end: the
// streamed response contains the text "PONG" and the stream completes
// normally.
func TestChatArrayDeltaStreamingE2E(t *testing.T) {
	ctx := context.Background()
	cfg := e2eMinimalConfig()
	hooks := format.CorePluginHooks{}.WithDefaults()
	reg := newTestRegistry(t, cfg, hooks)

	client, ok := reg.GetClient(configOpenAIResponse)
	if !ok {
		t.Fatal("client adapter not found")
	}
	clientStream, ok := reg.GetClientStream(configOpenAIResponse)
	if !ok {
		t.Fatal("client stream adapter not found")
	}
	provider, ok := reg.GetProvider(configOpenAIChat)
	if !ok {
		t.Fatal("provider adapter not found")
	}
	providerStream, ok := reg.GetProviderStream(configOpenAIChat)
	if !ok {
		t.Fatal("provider stream adapter not found")
	}

	// Mock upstream streaming Chat Completions server. It writes exactly
	// three data lines: the issue-shaped array-form chunk, the finish
	// chunk, and the SSE DONE marker, flushing after each so the bridge
	// consumes them as live stream events.
	mockSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "POST" {
			t.Errorf("method = %q, want POST", r.Method)
		}
		if r.URL.Path != "/v1/chat/completions" {
			t.Errorf("path = %q, want /v1/chat/completions", r.URL.Path)
		}

		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)

		// Line 1: issue-shaped array-form delta chunk (issue #11).
		fmt.Fprint(w, "data: "+`{"id":"1","object":"chat.completion.chunk","created":1,"model":"test-model","choices":[{"index":0,"delta":{"role":"assistant","content":[{"type":"thinking","thinking":[{"text":"hmm"}]},{"type":"text","text":"PONG"}]}}]}`+"\n\n")
		sseFlush(w)

		// Line 2: finish chunk with usage.
		fmt.Fprint(w, "data: "+`{"id":"2","object":"chat.completion.chunk","created":1,"model":"test-model","choices":[{"index":0,"delta":{},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`+"\n\n")
		sseFlush(w)

		// Line 3: SSE DONE marker, terminating the stream.
		fmt.Fprint(w, "data: "+"["+"DONE"+"]"+"\n\n")
		sseFlush(w)
	}))
	defer mockSrv.Close()

	// Step 1: Build streaming OpenAI Responses request (inbound).
	openAIReq := openai.ResponsesRequest{
		Model:           "gpt-4o",
		Input:           json.RawMessage(`"Hello array-form streaming"`),
		MaxOutputTokens: 100,
		Stream:          true,
	}

	// Step 2: ClientAdapter.ToCoreRequest.
	coreReq, err := client.ToCoreRequest(ctx, &openAIReq)
	if err != nil {
		t.Fatalf("ToCoreRequest: %v", err)
	}
	if !coreReq.Stream {
		t.Error("expected Stream=true in CoreRequest")
	}

	// Step 3: ProviderAdapter.FromCoreRequest.
	upstreamAny, err := provider.FromCoreRequest(ctx, coreReq)
	if err != nil {
		t.Fatalf("FromCoreRequest: %v", err)
	}
	upstreamReq, ok := upstreamAny.(*chat.ChatRequest)
	if !ok {
		t.Fatalf("FromCoreRequest returned %T, want *chat.ChatRequest", upstreamAny)
	}

	// Step 4: Stream from the mock upstream via the chat client.
	chatClient := chat.NewClient(chat.ClientConfig{
		BaseURL: mockSrv.URL,
		APIKey:  "test-key",
		Client:  mockSrv.Client(),
	})
	chunkCh, err := chatClient.StreamChat(ctx, upstreamReq)
	if err != nil {
		t.Fatalf("StreamChat: %v", err)
	}

	// Step 5: ProviderStreamAdapter.ToCoreStream.
	coreEvents, err := providerStream.ToCoreStream(ctx, chunkCh)
	if err != nil {
		t.Fatalf("ToCoreStream: %v", err)
	}

	// Step 6: ClientStreamAdapter.FromCoreStream.
	streamOutAny, err := clientStream.FromCoreStream(ctx, coreReq, coreEvents.Events)
	if err != nil {
		t.Fatalf("FromCoreStream: %v", err)
	}
	var openAIStream <-chan openai.StreamEvent
	oaiResult, ok := streamOutAny.(*openai.OpenAIStreamResult)
	if ok {
		openAIStream = oaiResult.Chan()
	} else {
		openAIStream, ok = streamOutAny.(<-chan openai.StreamEvent)
		if !ok {
			t.Fatalf("FromCoreStream returned %T, want *openai.OpenAIStreamResult or <-chan openai.StreamEvent", streamOutAny)
		}
	}

	// Consume the outbound OpenAI stream: collect text deltas and verify
	// the stream terminates with response.completed.
	var text strings.Builder
	var completed bool
	for ev := range openAIStream {
		switch ev.Event {
		case "response.output_text.delta":
			if delta, ok := ev.Data.(openai.OutputTextDeltaEvent); ok {
				text.WriteString(delta.Delta)
			}
		case "response.completed":
			completed = true
		}
	}

	// The array-form chunk's text block must reach the consumer.
	if got := text.String(); !strings.Contains(got, "PONG") {
		t.Errorf("streamed text = %q, want it to contain %q", got, "PONG")
	}
	if text.Len() == 0 {
		t.Error("streamed text is empty, want non-empty text content")
	}

	// The SSE stream must complete normally.
	if !completed {
		t.Error("stream did not complete: response.completed event not found")
	}
}
