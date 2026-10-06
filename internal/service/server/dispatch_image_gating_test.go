package server

// Server-package coverage for the image-capability gating in the Responses
// adapter dispatch (handleWithAdapters / handleAdapterStream). These tests
// call the unexported handlers directly so they can hand the dispatch a
// text-only candidate with an image request, which the HTTP entry point
// (handleResponses -> filterCandidatesByInput) would otherwise reject before
// dispatch. The critical regression class under test is the non-streaming
// anthropic fall-through: a request with NO image must reach CreateMessage and
// produce a normal response, never a nil-CoreResponse panic.

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"moonbridge/internal/config"
	"moonbridge/internal/format"
	"moonbridge/internal/protocol/anthropic"
	"moonbridge/internal/protocol/chat"
	"moonbridge/internal/protocol/openai"
	"moonbridge/internal/service/provider"

	visualpkg "moonbridge/internal/extension/visual"
)

// dispatchMock records raw upstream request bodies and answers both streaming
// and non-streaming requests.
type dispatchMock struct {
	mu     sync.Mutex
	bodies []string
}

func (m *dispatchMock) record(body string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.bodies = append(m.bodies, body)
}

func (m *dispatchMock) count() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.bodies)
}

func (m *dispatchMock) lastBody() string {
	m.mu.Lock()
	defer m.mu.Unlock()
	if len(m.bodies) == 0 {
		return ""
	}
	return m.bodies[len(m.bodies)-1]
}

// newDispatchAnthropicMock starts a mock Anthropic upstream.
func newDispatchAnthropicMock(t *testing.T) (*httptest.Server, *dispatchMock) {
	t.Helper()
	rec := &dispatchMock{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		rec.record(string(body))

		if strings.Contains(string(body), `"stream":true`) {
			w.Header().Set("Content-Type", "text/event-stream")
			w.WriteHeader(http.StatusOK)
			fmt.Fprint(w, "event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"id\":\"m1\",\"type\":\"message\",\"role\":\"assistant\",\"content\":[],\"usage\":{\"input_tokens\":1,\"output_tokens\":0}}}\n\n")
			fmt.Fprint(w, "event: content_block_start\ndata: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"text\",\"text\":\"\"}}\n\n")
			fmt.Fprint(w, "event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\"anthropic stream\"}}\n\n")
			fmt.Fprint(w, "event: content_block_stop\ndata: {\"type\":\"content_block_stop\",\"index\":0}\n\n")
			fmt.Fprint(w, "event: message_delta\ndata: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"end_turn\"},\"usage\":{\"input_tokens\":1,\"output_tokens\":2}}\n\n")
			fmt.Fprint(w, "event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n")
			return
		}

		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		fmt.Fprint(w, `{"id":"msg_dispatch","type":"message","role":"assistant","content":[{"type":"text","text":"anthropic ok"}],"model":"mock","stop_reason":"end_turn","usage":{"input_tokens":1,"output_tokens":1}}`)
	}))
	t.Cleanup(srv.Close)
	return srv, rec
}

// newDispatchChatMock starts a mock OpenAI-chat upstream.
func newDispatchChatMock(t *testing.T) (*httptest.Server, *dispatchMock) {
	t.Helper()
	rec := &dispatchMock{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		rec.record(string(body))

		var req struct {
			Stream bool `json:"stream"`
		}
		_ = json.Unmarshal(body, &req)

		if req.Stream {
			w.Header().Set("Content-Type", "text/event-stream")
			flusher, _ := w.(http.Flusher)
			fmt.Fprint(w, "data: {\"id\":\"c1\",\"object\":\"chat.completion.chunk\",\"model\":\"mock\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"chat stream\"},\"finish_reason\":null}]}\n\n")
			fmt.Fprint(w, "data: {\"id\":\"c1\",\"object\":\"chat.completion.chunk\",\"model\":\"mock\",\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"stop\"}]}\n\n")
			fmt.Fprint(w, "data: [DONE]\n\n")
			if flusher != nil {
				flusher.Flush()
			}
			return
		}

		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		fmt.Fprint(w, `{"id":"chatcmpl-dispatch","object":"chat.completion","created":1,"model":"mock","choices":[{"index":0,"message":{"role":"assistant","content":"chat ok"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`)
	}))
	t.Cleanup(srv.Close)
	return srv, rec
}

// newDispatchProviderManager builds a one-provider manager whose model
// modalities drive candidateSupportsImage.
func newDispatchProviderManager(t *testing.T, protocol, baseURL string, modalities []string) *provider.ProviderManager {
	t.Helper()
	pm, err := provider.NewProviderManager(
		map[string]provider.ProviderConfig{
			"main": {
				BaseURL:  baseURL,
				APIKey:   "k",
				Protocol: protocol,
				Models: map[string]provider.ModelMeta{
					"upstream-model": {InputModalities: modalities},
				},
			},
		},
		map[string]provider.ModelRoute{
			"alias": {Provider: "main", Name: "upstream-model"},
		},
	)
	if err != nil {
		t.Fatalf("NewProviderManager() error = %v", err)
	}
	return pm
}

// dispatchNoopCache satisfies anthropic.CacheManager for the provider adapter.
type dispatchNoopCache struct{}

func (dispatchNoopCache) PlanAndInject(_ context.Context, _ *anthropic.MessageRequest, _ *format.CoreRequest) (string, string) {
	return "", ""
}

func (dispatchNoopCache) UpdateRegistry(_ context.Context, _, _ string, _ anthropic.Usage) {}

// newDispatchRegistry builds a registry with the Responses inbound client and
// the requested provider protocols.
func newDispatchRegistry(t *testing.T, protocols ...string) *format.Registry {
	t.Helper()
	hooks := format.CorePluginHooks{}.WithDefaults()
	reg := format.NewRegistry()
	oai := openai.NewOpenAIAdapter(hooks)
	if err := reg.RegisterClient(oai); err != nil {
		t.Fatalf("RegisterClient(openai-response): %v", err)
	}
	if err := reg.RegisterClientStream(oai); err != nil {
		t.Fatalf("RegisterClientStream(openai-response): %v", err)
	}
	for _, protocol := range protocols {
		switch protocol {
		case config.ProtocolAnthropic:
			adapter := anthropic.NewAnthropicProviderAdapter(1024, dispatchNoopCache{}, hooks)
			if err := reg.RegisterProvider(adapter); err != nil {
				t.Fatalf("RegisterProvider(anthropic): %v", err)
			}
			if err := reg.RegisterProviderStream(adapter); err != nil {
				t.Fatalf("RegisterProviderStream(anthropic): %v", err)
			}
		case config.ProtocolOpenAIChat:
			adapter := chat.NewChatProviderAdapter(1024, nil, hooks)
			if err := reg.RegisterProvider(adapter); err != nil {
				t.Fatalf("RegisterProvider(openai-chat): %v", err)
			}
			if err := reg.RegisterProviderStream(adapter); err != nil {
				t.Fatalf("RegisterProviderStream(openai-chat): %v", err)
			}
		}
	}
	return reg
}

// responsesImageInput is a message item whose content carries an image.
func responsesImageInput() json.RawMessage {
	return json.RawMessage(`[{"role":"user","content":[{"type":"input_text","text":"describe"},{"type":"input_image","image_url":"data:image/png;base64,AAAA"}]}]`)
}

// responsesTextInput is a plain text input.
func responsesTextInput() json.RawMessage {
	return json.RawMessage(`"hello"`)
}

// runHandleWithAdapters invokes handleWithAdapters directly and returns the
// recorded status and body.
func runHandleWithAdapters(t *testing.T, srv *Server, input json.RawMessage, stream bool) (int, string) {
	t.Helper()
	req := openai.ResponsesRequest{Model: "alias", Input: input, Stream: stream}
	route, err := srv.activeProviderManager().ResolveModel("alias")
	if err != nil {
		t.Fatalf("ResolveModel() error = %v", err)
	}
	if len(route.Candidates) == 0 {
		t.Fatal("no candidates")
	}

	body, _ := json.Marshal(req)
	recorder := httptest.NewRecorder()
	httpReq := httptest.NewRequest(http.MethodPost, "/v1/responses", bytes.NewReader(body))
	srv.handleWithAdapters(recorder, httpReq, req, route)
	return recorder.Code, recorder.Body.String()
}

// ============================================================================
// Anthropic upstream
// ============================================================================

// TestHandleWithAdaptersAnthropicNonStreamNoImageFallsThrough is the exact
// regression guard the reviewer flagged: a non-streaming Responses request
// with no image must reach CreateMessage and return a normal response rather
// than a nil-CoreResponse panic.
func TestHandleWithAdaptersAnthropicNonStreamNoImageFallsThrough(t *testing.T) {
	upstream, rec := newDispatchAnthropicMock(t)
	pm := newDispatchProviderManager(t, config.ProtocolAnthropic, upstream.URL, []string{"text"})
	srv := New(Config{ProviderMgr: pm, AdapterRegistry: newDispatchRegistry(t, config.ProtocolAnthropic)})

	status, body := runHandleWithAdapters(t, srv, responsesTextInput(), false)
	if status != http.StatusOK {
		t.Fatalf("status = %d, body = %s", status, body)
	}
	if !strings.Contains(body, "anthropic ok") {
		t.Fatalf("response body missing upstream text: %s", body)
	}
	if rec.count() != 1 {
		t.Fatalf("upstream call count = %d, want 1", rec.count())
	}
}

// TestHandleWithAdaptersAnthropicNonStreamImageTextOnlyStripsOnFallThrough
// proves the strip applies only on the orchestrator fall-through when the
// candidate cannot consume images.
func TestHandleWithAdaptersAnthropicNonStreamImageTextOnlyStripsOnFallThrough(t *testing.T) {
	upstream, rec := newDispatchAnthropicMock(t)
	pm := newDispatchProviderManager(t, config.ProtocolAnthropic, upstream.URL, []string{"text"})
	srv := New(Config{ProviderMgr: pm, AdapterRegistry: newDispatchRegistry(t, config.ProtocolAnthropic)})

	status, body := runHandleWithAdapters(t, srv, responsesImageInput(), false)
	if status != http.StatusOK {
		t.Fatalf("status = %d, body = %s", status, body)
	}
	upstreamBody := rec.lastBody()
	if !strings.Contains(upstreamBody, "Image #1 is available to Visual Brief and Visual QA") {
		t.Fatalf("upstream body missing the visual strip placeholder: %s", upstreamBody)
	}
	if strings.Contains(upstreamBody, "AAAA") {
		t.Fatalf("upstream body still contains the raw image data: %s", upstreamBody)
	}
}

// TestHandleWithAdaptersAnthropicNonStreamImageCapableForwardsImage proves an
// image-capable candidate receives the image unstripped (no orchestrator, no
// strip).
func TestHandleWithAdaptersAnthropicNonStreamImageCapableForwardsImage(t *testing.T) {
	upstream, rec := newDispatchAnthropicMock(t)
	pm := newDispatchProviderManager(t, config.ProtocolAnthropic, upstream.URL, []string{"text", "image"})
	srv := New(Config{ProviderMgr: pm, AdapterRegistry: newDispatchRegistry(t, config.ProtocolAnthropic)})

	status, body := runHandleWithAdapters(t, srv, responsesImageInput(), false)
	if status != http.StatusOK {
		t.Fatalf("status = %d, body = %s", status, body)
	}
	upstreamBody := rec.lastBody()
	if !strings.Contains(upstreamBody, "AAAA") {
		t.Fatalf("upstream body missing the image data: %s", upstreamBody)
	}
	if strings.Contains(upstreamBody, "Image #1 is available to Visual Brief and Visual QA") {
		t.Fatalf("upstream body contains the visual strip placeholder: %s", upstreamBody)
	}
	if rec.count() != 1 {
		t.Fatalf("upstream call count = %d, want 1", rec.count())
	}
}

// TestHandleAdapterStreamAnthropicNoImageStreamsDirectly covers
// handleAdapterStream's anthropic branch for a text-only request with no
// image (no strip, direct StreamMessage).
func TestHandleAdapterStreamAnthropicNoImageStreamsDirectly(t *testing.T) {
	upstream, rec := newDispatchAnthropicMock(t)
	pm := newDispatchProviderManager(t, config.ProtocolAnthropic, upstream.URL, []string{"text"})
	srv := New(Config{ProviderMgr: pm, AdapterRegistry: newDispatchRegistry(t, config.ProtocolAnthropic)})

	status, body := runHandleWithAdapters(t, srv, responsesTextInput(), true)
	if status != http.StatusOK {
		t.Fatalf("status = %d, body = %s", status, body)
	}
	if !strings.Contains(body, "anthropic stream") {
		t.Fatalf("stream body missing upstream text: %s", body)
	}
	upstreamBody := rec.lastBody()
	if strings.Contains(upstreamBody, "Image #1") {
		t.Fatalf("text-only upstream body contains a strip placeholder: %s", upstreamBody)
	}
}

// TestHandleAdapterStreamAnthropicImageTextOnlyStripsOnFallThrough covers
// handleAdapterStream's anthropic strip fall-through.
func TestHandleAdapterStreamAnthropicImageTextOnlyStripsOnFallThrough(t *testing.T) {
	upstream, rec := newDispatchAnthropicMock(t)
	pm := newDispatchProviderManager(t, config.ProtocolAnthropic, upstream.URL, []string{"text"})
	srv := New(Config{ProviderMgr: pm, AdapterRegistry: newDispatchRegistry(t, config.ProtocolAnthropic)})

	status, body := runHandleWithAdapters(t, srv, responsesImageInput(), true)
	if status != http.StatusOK {
		t.Fatalf("status = %d, body = %s", status, body)
	}
	if !strings.Contains(body, "anthropic stream") {
		t.Fatalf("stream body missing upstream text: %s", body)
	}
	upstreamBody := rec.lastBody()
	if !strings.Contains(upstreamBody, "Image #1 is available to Visual Brief and Visual QA") {
		t.Fatalf("upstream body missing the visual strip placeholder: %s", upstreamBody)
	}
	if strings.Contains(upstreamBody, "AAAA") {
		t.Fatalf("upstream body still contains the raw image data: %s", upstreamBody)
	}
}

// ============================================================================
// OpenAI-chat upstream
// ============================================================================

// TestHandleWithAdaptersChatNonStreamNoImageFallsThrough covers the chat
// non-streaming fall-through with no image.
func TestHandleWithAdaptersChatNonStreamNoImageFallsThrough(t *testing.T) {
	upstream, rec := newDispatchChatMock(t)
	pm := newDispatchProviderManager(t, config.ProtocolOpenAIChat, upstream.URL, []string{"text"})
	srv := New(Config{
		ProviderMgr:     pm,
		AdapterRegistry: newDispatchRegistry(t, config.ProtocolOpenAIChat),
		ChatClients:     map[string]any{"main": chat.NewClient(chat.ClientConfig{BaseURL: upstream.URL, APIKey: "k"})},
	})

	status, body := runHandleWithAdapters(t, srv, responsesTextInput(), false)
	if status != http.StatusOK {
		t.Fatalf("status = %d, body = %s", status, body)
	}
	if !strings.Contains(body, "chat ok") {
		t.Fatalf("response body missing upstream text: %s", body)
	}
	if rec.count() != 1 {
		t.Fatalf("upstream call count = %d, want 1", rec.count())
	}
}

// TestHandleWithAdaptersChatNonStreamImageTextOnlyStripsOnFallThrough covers
// the chat non-streaming strip fall-through.
func TestHandleWithAdaptersChatNonStreamImageTextOnlyStripsOnFallThrough(t *testing.T) {
	upstream, rec := newDispatchChatMock(t)
	pm := newDispatchProviderManager(t, config.ProtocolOpenAIChat, upstream.URL, []string{"text"})
	srv := New(Config{
		ProviderMgr:     pm,
		AdapterRegistry: newDispatchRegistry(t, config.ProtocolOpenAIChat),
		ChatClients:     map[string]any{"main": chat.NewClient(chat.ClientConfig{BaseURL: upstream.URL, APIKey: "k"})},
	})

	status, body := runHandleWithAdapters(t, srv, responsesImageInput(), false)
	if status != http.StatusOK {
		t.Fatalf("status = %d, body = %s", status, body)
	}
	upstreamBody := rec.lastBody()
	if !strings.Contains(upstreamBody, "Image #1 is available to Visual Brief and Visual QA") {
		t.Fatalf("upstream body missing the visual strip placeholder: %s", upstreamBody)
	}
	if strings.Contains(upstreamBody, "AAAA") {
		t.Fatalf("upstream body still contains the raw image data: %s", upstreamBody)
	}
}

// TestHandleWithAdaptersChatNonStreamImageCapableForwardsImage covers the
// image-capable chat candidate: image forwarded unstripped.
func TestHandleWithAdaptersChatNonStreamImageCapableForwardsImage(t *testing.T) {
	upstream, rec := newDispatchChatMock(t)
	pm := newDispatchProviderManager(t, config.ProtocolOpenAIChat, upstream.URL, []string{"text", "image"})
	srv := New(Config{
		ProviderMgr:     pm,
		AdapterRegistry: newDispatchRegistry(t, config.ProtocolOpenAIChat),
		ChatClients:     map[string]any{"main": chat.NewClient(chat.ClientConfig{BaseURL: upstream.URL, APIKey: "k"})},
	})

	status, body := runHandleWithAdapters(t, srv, responsesImageInput(), false)
	if status != http.StatusOK {
		t.Fatalf("status = %d, body = %s", status, body)
	}
	upstreamBody := rec.lastBody()
	if !strings.Contains(upstreamBody, "AAAA") {
		t.Fatalf("upstream body missing the image data: %s", upstreamBody)
	}
	if strings.Contains(upstreamBody, "Image #1 is available to Visual Brief and Visual QA") {
		t.Fatalf("upstream body contains the visual strip placeholder: %s", upstreamBody)
	}
}

// TestHandleAdapterStreamChatImageTextOnlyStripsOnFallThrough covers
// handleAdapterStream's chat strip fall-through.
func TestHandleAdapterStreamChatImageTextOnlyStripsOnFallThrough(t *testing.T) {
	upstream, rec := newDispatchChatMock(t)
	pm := newDispatchProviderManager(t, config.ProtocolOpenAIChat, upstream.URL, []string{"text"})
	srv := New(Config{
		ProviderMgr:     pm,
		AdapterRegistry: newDispatchRegistry(t, config.ProtocolOpenAIChat),
		ChatClients:     map[string]any{"main": chat.NewClient(chat.ClientConfig{BaseURL: upstream.URL, APIKey: "k"})},
	})

	status, body := runHandleWithAdapters(t, srv, responsesImageInput(), true)
	if status != http.StatusOK {
		t.Fatalf("status = %d, body = %s", status, body)
	}
	if !strings.Contains(body, "chat stream") {
		t.Fatalf("stream body missing upstream text: %s", body)
	}
	upstreamBody := rec.lastBody()
	if !strings.Contains(upstreamBody, "Image #1 is available to Visual Brief and Visual QA") {
		t.Fatalf("upstream body missing the visual strip placeholder: %s", upstreamBody)
	}
}

// Ensure the visual strip helper used by the assertions is the production
// placeholder (guards against the wording drifting out of sync).
func TestDispatchStripPlaceholderMatchesProduction(t *testing.T) {
	stripped, changed := visualpkg.StripImagesFromChat(chat.ChatRequest{
		Model: "m",
		Messages: []chat.ChatMessage{{
			Role: "user",
			Content: []chat.ContentPart{
				{Type: "text", Text: "describe"},
				{Type: "image_url", ImageURL: &chat.ImageURL{URL: "data:image/png;base64,AAAA"}},
			},
		}},
	})
	if !changed {
		t.Fatal("StripImagesFromChat reported no change for an image request")
	}
	raw, _ := json.Marshal(stripped)
	if !strings.Contains(string(raw), "Image #1 is available to Visual Brief and Visual QA") {
		t.Fatalf("strip placeholder wording changed: %s", raw)
	}
}
