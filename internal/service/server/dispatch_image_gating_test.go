package server

// Server-package coverage for the image-capability gating in the Responses
// adapter dispatch (handleWithAdapters / handleAdapterStream). These tests
// call the unexported handlers directly. Contrary to an earlier note here,
// the HTTP entry point does NOT reliably filter nested-format image requests
// away from text-only candidates: requestHasImage (server.go) inspects only
// TOP-LEVEL input items for a "type" of input_image/image/image_url, while the
// real Responses wire format nests the image in
// {"role":"user","content":[{"type":"input_image",...}]}. A nested-format
// image request therefore survives filterCandidatesByInput unchanged, reaches
// dispatch with a text-only candidate, and (when a visual orchestrator is
// configured) executes the orchestrator branch. (A top-level input_image item
// *is* detected and filtered; only the nested shape slips through.) The other
// critical regression class under test is the non-streaming anthropic
// fall-through: a request with NO image must reach CreateMessage and produce a
// normal response, never a nil-CoreResponse panic.

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"moonbridge/internal/config"
	"moonbridge/internal/extension/plugin"
	"moonbridge/internal/format"
	"moonbridge/internal/protocol/anthropic"
	"moonbridge/internal/protocol/chat"
	"moonbridge/internal/protocol/openai"
	"moonbridge/internal/service/provider"
	"moonbridge/internal/service/runtime"

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

func (m *dispatchMock) firstBody() string {
	m.mu.Lock()
	defer m.mu.Unlock()
	if len(m.bodies) == 0 {
		return ""
	}
	return m.bodies[0]
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
// the requested provider protocols, using the default plugin hooks.
func newDispatchRegistry(t *testing.T, protocols ...string) *format.Registry {
	t.Helper()
	return newDispatchRegistryWithHooks(t, format.CorePluginHooks{}.WithDefaults(), protocols...)
}

// newDispatchRegistryWithHooks builds the registry described above with
// explicit plugin hooks (used by the visual-orchestrator harness so the
// registered adapters share the plugin registry's hooks).
func newDispatchRegistryWithHooks(t *testing.T, hooks format.CorePluginHooks, protocols ...string) *format.Registry {
	t.Helper()
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

// ============================================================================
// Visual-orchestrator-runs branches (handleWithAdapters / handleAdapterStream)
// ============================================================================
//
// Unlike the tests above (which construct a Server with no Runtime or
// PluginRegistry, so wrapWithVisual always returns nil and only the strip
// fall-through runs), the tests below wire a full Server: a Runtime whose
// resolved config enables the visual extension for "alias" and points it at a
// resolvable vision provider, a plugin.Registry holding the visual plugin, and
// a mock vision upstream. That makes wrapWithVisual return non-nil, so the
// orchestrator-runs branches execute.

const dispatchVisionBriefMarker = "VISION-BRIEF-MARKER"

// dispatchVisualConfig builds a config.Config whose "alias" route enables the
// visual extension and resolves to vision-provider/vision-model.
func dispatchVisualConfig(mainProtocol, mainBaseURL, visionBaseURL string, mainModalities []string) config.Config {
	enabled := true
	return config.Config{
		Routes: map[string]config.RouteEntry{
			"alias": {
				Provider:        "main",
				Model:           "upstream-model",
				InputModalities: mainModalities,
				Extensions: map[string]config.ExtensionSettings{
					"visual": {
						Enabled:   &enabled,
						RawConfig: map[string]any{"provider": "vision-provider", "model": "vision-model"},
					},
				},
			},
		},
		ProviderDefs: map[string]config.ProviderDef{
			"main": {
				BaseURL:  mainBaseURL,
				APIKey:   "k",
				Protocol: mainProtocol,
				Models: map[string]config.ModelMeta{
					"upstream-model": {InputModalities: mainModalities},
				},
			},
			"vision-provider": {
				BaseURL:  visionBaseURL,
				APIKey:   "k",
				Protocol: config.ProtocolAnthropic,
				Models: map[string]config.ModelMeta{
					"vision-model": {InputModalities: []string{"text", "image"}},
				},
			},
		},
	}
}

// newDispatchVisualManager builds a provider manager with a routed "main"
// model plus a resolvable anthropic vision provider.
func newDispatchVisualManager(t *testing.T, mainProtocol, mainBaseURL, visionBaseURL string, mainModalities []string) *provider.ProviderManager {
	t.Helper()
	pm, err := provider.NewProviderManager(
		map[string]provider.ProviderConfig{
			"main": {
				BaseURL:  mainBaseURL,
				APIKey:   "k",
				Protocol: mainProtocol,
				Models: map[string]provider.ModelMeta{
					"upstream-model": {InputModalities: mainModalities},
				},
			},
			"vision-provider": {
				BaseURL:  visionBaseURL,
				APIKey:   "k",
				Protocol: config.ProtocolAnthropic,
				Models: map[string]provider.ModelMeta{
					"vision-model": {InputModalities: []string{"text", "image"}},
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

// newDispatchVisualServer wires a Server with the full visual orchestration
// machinery: Runtime (visual extension enabled for "alias"), plugin registry
// with the visual plugin, both protocol adapters, and (for chat) a chat client.
func newDispatchVisualServer(t *testing.T, mainProtocol, mainBaseURL, visionBaseURL string, mainModalities []string) *Server {
	t.Helper()
	pm := newDispatchVisualManager(t, mainProtocol, mainBaseURL, visionBaseURL, mainModalities)
	cfg := dispatchVisualConfig(mainProtocol, mainBaseURL, visionBaseURL, mainModalities)

	pluginReg := plugin.NewRegistry(slog.Default())
	pluginReg.Register(visualpkg.NewPlugin())
	pluginReg.SetCurrentConfigProvider(func() config.Config { return cfg })
	if err := pluginReg.InitAll(&cfg); err != nil {
		t.Fatalf("InitAll() error = %v", err)
	}

	reg := newDispatchRegistryWithHooks(t, pluginReg.CorePluginHooks(), config.ProtocolAnthropic, config.ProtocolOpenAIChat)

	chatClients := map[string]any{}
	if mainProtocol == config.ProtocolOpenAIChat {
		chatClients["main"] = chat.NewClient(chat.ClientConfig{BaseURL: mainBaseURL, APIKey: "k"})
	}

	return New(Config{
		ProviderMgr:     pm,
		AdapterRegistry: reg,
		ChatClients:     chatClients,
		PluginRegistry:  pluginReg,
		Runtime:         runtime.NewRuntime(cfg, pm, nil),
	})
}

// newDispatchAnthropicOrchestratorMock starts a mock Anthropic upstream that
// drives the visual orchestrator on its first non-streaming call (a
// visual_brief tool_use) and answers follow-up rounds with a text completion
// that echoes the vision-brief marker when it sees it in the request. Streaming
// requests (only expected on the direct, non-orchestrator path) get valid SSE.
func newDispatchAnthropicOrchestratorMock(t *testing.T) (*httptest.Server, *dispatchMock) {
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
		if rec.count() == 1 {
			fmt.Fprint(w, `{"id":"msg_tool","type":"message","role":"assistant","content":[{"type":"tool_use","id":"call_visual_1","name":"visual_brief","input":{"image_refs":["Image #1"],"context":"describe"}}],"model":"mock","stop_reason":"tool_use","usage":{"input_tokens":1,"output_tokens":1}}`)
			return
		}
		finalText := "orchestrated final answer"
		if strings.Contains(string(body), dispatchVisionBriefMarker) {
			finalText = "orchestrated final answer " + dispatchVisionBriefMarker
		}
		payload, _ := json.Marshal(map[string]any{
			"id":          "msg_final",
			"type":        "message",
			"role":        "assistant",
			"content":     []any{map[string]any{"type": "text", "text": finalText}},
			"model":       "mock",
			"stop_reason": "end_turn",
			"usage":       map[string]any{"input_tokens": 1, "output_tokens": 1},
		})
		_, _ = w.Write(payload)
	}))
	t.Cleanup(srv.Close)
	return srv, rec
}

// newDispatchAnthropicVisionMock starts a mock Anthropic vision provider that
// always answers with a text block carrying dispatchVisionBriefMarker.
func newDispatchAnthropicVisionMock(t *testing.T) (*httptest.Server, *dispatchMock) {
	t.Helper()
	rec := &dispatchMock{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		rec.record(string(body))
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		payload, _ := json.Marshal(map[string]any{
			"id":          "msg_vision",
			"type":        "message",
			"role":        "assistant",
			"content":     []any{map[string]any{"type": "text", "text": dispatchVisionBriefMarker}},
			"model":       "vision-model",
			"stop_reason": "end_turn",
			"usage":       map[string]any{"input_tokens": 1, "output_tokens": 1},
		})
		_, _ = w.Write(payload)
	}))
	t.Cleanup(srv.Close)
	return srv, rec
}

// newDispatchChatOrchestratorMock starts a mock OpenAI-chat upstream that
// drives the visual orchestrator on its first non-streaming call (a
// visual_brief tool_call) and answers follow-up rounds with a text completion.
func newDispatchChatOrchestratorMock(t *testing.T) (*httptest.Server, *dispatchMock) {
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
		if rec.count() == 1 {
			fmt.Fprint(w, `{"id":"chatcmpl-tool","object":"chat.completion","created":1,"model":"mock","choices":[{"index":0,"message":{"role":"assistant","content":null,"tool_calls":[{"id":"call_visual_1","type":"function","function":{"name":"visual_brief","arguments":"{\"image_refs\":[\"Image #1\"],\"context\":\"describe\"}"}}]},"finish_reason":"tool_calls"}],"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`)
			return
		}
		finalText := "orchestrated final answer"
		if strings.Contains(string(body), dispatchVisionBriefMarker) {
			finalText = "orchestrated final answer " + dispatchVisionBriefMarker
		}
		payload, _ := json.Marshal(map[string]any{
			"id":      "chatcmpl-final",
			"object":  "chat.completion",
			"created": 1,
			"model":   "mock",
			"choices": []any{map[string]any{
				"index":         0,
				"message":       map[string]any{"role": "assistant", "content": finalText},
				"finish_reason": "stop",
			}},
			"usage": map[string]any{"prompt_tokens": 1, "completion_tokens": 1, "total_tokens": 2},
		})
		_, _ = w.Write(payload)
	}))
	t.Cleanup(srv.Close)
	return srv, rec
}

// TestHandleWithAdaptersAnthropicNonStreamVisualOrchestratorRuns covers the
// orchestrator-runs branch of handleWithAdapters for an anthropic upstream:
// (a) the vision mock is called and the main upstream round is stripped,
// (c) the final body is the orchestrator's answer with the vision brief fed back.
func TestHandleWithAdaptersAnthropicNonStreamVisualOrchestratorRuns(t *testing.T) {
	upstream, rec := newDispatchAnthropicOrchestratorMock(t)
	vision, vrec := newDispatchAnthropicVisionMock(t)
	srv := newDispatchVisualServer(t, config.ProtocolAnthropic, upstream.URL, vision.URL, []string{"text"})

	status, body := runHandleWithAdapters(t, srv, responsesImageInput(), false)
	if status != http.StatusOK {
		t.Fatalf("status = %d, body = %s", status, body)
	}
	// (a) orchestrator ran.
	if vrec.count() != 1 {
		t.Fatalf("vision provider call count = %d, want 1 (orchestrator must run)", vrec.count())
	}
	if !strings.Contains(vrec.lastBody(), "AAAA") {
		t.Fatalf("vision provider body missing the forwarded image: %s", vrec.lastBody())
	}
	first := rec.firstBody()
	if !strings.Contains(first, "Image #1 is available to Visual Brief and Visual QA") {
		t.Fatalf("orchestrator first upstream body missing the visual placeholder: %s", first)
	}
	if strings.Contains(first, "AAAA") {
		t.Fatalf("orchestrator first upstream body still contains raw base64: %s", first)
	}
	if rec.count() < 2 {
		t.Fatalf("upstream call count = %d, want at least 2 (tool round + final)", rec.count())
	}
	// (c) final body is the orchestrator's answer, and the vision brief was fed back.
	if !strings.Contains(body, "orchestrated final answer") {
		t.Fatalf("final body missing orchestrator output: %s", body)
	}
	if !strings.Contains(body, dispatchVisionBriefMarker) {
		t.Fatalf("final body missing the vision brief marker: %s", body)
	}
	if !strings.Contains(rec.lastBody(), dispatchVisionBriefMarker) {
		t.Fatalf("follow-up upstream body missing the vision brief: %s", rec.lastBody())
	}
}

// TestHandleWithAdaptersAnthropicNonStreamImageCapableSkipsOrchestrator covers
// (b): an image-capable anthropic candidate forwards the image and never calls
// the vision provider, even though the visual extension is configured.
func TestHandleWithAdaptersAnthropicNonStreamImageCapableSkipsOrchestrator(t *testing.T) {
	upstream, rec := newDispatchAnthropicMock(t)
	vision, vrec := newDispatchAnthropicVisionMock(t)
	srv := newDispatchVisualServer(t, config.ProtocolAnthropic, upstream.URL, vision.URL, []string{"text", "image"})

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
	if vrec.count() != 0 {
		t.Fatalf("vision provider call count = %d, want 0 (image-capable upstream)", vrec.count())
	}
	if rec.count() != 1 {
		t.Fatalf("upstream call count = %d, want 1", rec.count())
	}
}

// TestHandleAdapterStreamAnthropicVisualOrchestratorRuns covers the
// orchestrator-runs branch of handleAdapterStream for an anthropic upstream
// (non-streaming orchestration synthesised into the client SSE stream):
// (a) vision called + first upstream round stripped, (c) orchestrator output
// in the stream.
func TestHandleAdapterStreamAnthropicVisualOrchestratorRuns(t *testing.T) {
	upstream, rec := newDispatchAnthropicOrchestratorMock(t)
	vision, vrec := newDispatchAnthropicVisionMock(t)
	srv := newDispatchVisualServer(t, config.ProtocolAnthropic, upstream.URL, vision.URL, []string{"text"})

	status, body := runHandleWithAdapters(t, srv, responsesImageInput(), true)
	if status != http.StatusOK {
		t.Fatalf("status = %d, body = %s", status, body)
	}
	// (a) orchestrator ran.
	if vrec.count() != 1 {
		t.Fatalf("vision provider call count = %d, want 1 (orchestrator must run)", vrec.count())
	}
	first := rec.firstBody()
	if !strings.Contains(first, "Image #1 is available to Visual Brief and Visual QA") {
		t.Fatalf("orchestrator first upstream body missing the visual placeholder: %s", first)
	}
	if strings.Contains(first, "AAAA") {
		t.Fatalf("orchestrator first upstream body still contains raw base64: %s", first)
	}
	if rec.count() < 2 {
		t.Fatalf("upstream call count = %d, want at least 2 (tool round + final)", rec.count())
	}
	// (c) the synthesised stream carries the orchestrator's answer, not the
	// plain upstream stream.
	if !strings.Contains(body, "orchestrated final answer") {
		t.Fatalf("stream body missing orchestrator output: %s", body)
	}
	if !strings.Contains(body, dispatchVisionBriefMarker) {
		t.Fatalf("stream body missing the vision brief marker: %s", body)
	}
	if strings.Contains(body, "anthropic stream") {
		t.Fatalf("stream body used the plain upstream stream instead of the orchestrator: %s", body)
	}
}

// TestHandleWithAdaptersChatNonStreamVisualOrchestratorRuns covers the
// orchestrator-runs branch of handleWithAdapters for an openai-chat upstream:
// (a) vision called + first upstream round stripped, (c) orchestrator output.
func TestHandleWithAdaptersChatNonStreamVisualOrchestratorRuns(t *testing.T) {
	upstream, rec := newDispatchChatOrchestratorMock(t)
	vision, vrec := newDispatchAnthropicVisionMock(t)
	srv := newDispatchVisualServer(t, config.ProtocolOpenAIChat, upstream.URL, vision.URL, []string{"text"})

	status, body := runHandleWithAdapters(t, srv, responsesImageInput(), false)
	if status != http.StatusOK {
		t.Fatalf("status = %d, body = %s", status, body)
	}
	// (a) orchestrator ran.
	if vrec.count() != 1 {
		t.Fatalf("vision provider call count = %d, want 1 (orchestrator must run)", vrec.count())
	}
	if !strings.Contains(vrec.lastBody(), "AAAA") {
		t.Fatalf("vision provider body missing the forwarded image: %s", vrec.lastBody())
	}
	first := rec.firstBody()
	if !strings.Contains(first, "Image #1 is available to Visual Brief and Visual QA") {
		t.Fatalf("orchestrator first upstream body missing the visual placeholder: %s", first)
	}
	if strings.Contains(first, "AAAA") {
		t.Fatalf("orchestrator first upstream body still contains raw base64: %s", first)
	}
	if rec.count() < 2 {
		t.Fatalf("upstream call count = %d, want at least 2 (tool round + final)", rec.count())
	}
	// (c) final body is the orchestrator's answer, and the vision brief was fed back.
	if !strings.Contains(body, "orchestrated final answer") {
		t.Fatalf("final body missing orchestrator output: %s", body)
	}
	if !strings.Contains(body, dispatchVisionBriefMarker) {
		t.Fatalf("final body missing the vision brief marker: %s", body)
	}
	if !strings.Contains(rec.lastBody(), dispatchVisionBriefMarker) {
		t.Fatalf("follow-up upstream body missing the vision brief: %s", rec.lastBody())
	}
}

// TestHandleWithAdaptersChatNonStreamImageCapableSkipsOrchestrator covers (b)
// for an image-capable openai-chat candidate: image forwarded, vision unused.
func TestHandleWithAdaptersChatNonStreamImageCapableSkipsOrchestrator(t *testing.T) {
	upstream, rec := newDispatchChatMock(t)
	vision, vrec := newDispatchAnthropicVisionMock(t)
	srv := newDispatchVisualServer(t, config.ProtocolOpenAIChat, upstream.URL, vision.URL, []string{"text", "image"})

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
	if vrec.count() != 0 {
		t.Fatalf("vision provider call count = %d, want 0 (image-capable upstream)", vrec.count())
	}
	if rec.count() != 1 {
		t.Fatalf("upstream call count = %d, want 1", rec.count())
	}
}

// TestHandleAdapterStreamChatVisualOrchestratorRuns covers the
// orchestrator-runs branch of handleAdapterStream for an openai-chat upstream:
// (a) vision called + first upstream round stripped, (c) orchestrator output
// in the synthesised SSE stream.
func TestHandleAdapterStreamChatVisualOrchestratorRuns(t *testing.T) {
	upstream, rec := newDispatchChatOrchestratorMock(t)
	vision, vrec := newDispatchAnthropicVisionMock(t)
	srv := newDispatchVisualServer(t, config.ProtocolOpenAIChat, upstream.URL, vision.URL, []string{"text"})

	status, body := runHandleWithAdapters(t, srv, responsesImageInput(), true)
	if status != http.StatusOK {
		t.Fatalf("status = %d, body = %s", status, body)
	}
	// (a) orchestrator ran.
	if vrec.count() != 1 {
		t.Fatalf("vision provider call count = %d, want 1 (orchestrator must run)", vrec.count())
	}
	first := rec.firstBody()
	if !strings.Contains(first, "Image #1 is available to Visual Brief and Visual QA") {
		t.Fatalf("orchestrator first upstream body missing the visual placeholder: %s", first)
	}
	if strings.Contains(first, "AAAA") {
		t.Fatalf("orchestrator first upstream body still contains raw base64: %s", first)
	}
	if rec.count() < 2 {
		t.Fatalf("upstream call count = %d, want at least 2 (tool round + final)", rec.count())
	}
	// (c) the synthesised stream carries the orchestrator's answer, not the
	// plain upstream stream.
	if !strings.Contains(body, "orchestrated final answer") {
		t.Fatalf("stream body missing orchestrator output: %s", body)
	}
	if !strings.Contains(body, dispatchVisionBriefMarker) {
		t.Fatalf("stream body missing the vision brief marker: %s", body)
	}
	if strings.Contains(body, "chat stream") {
		t.Fatalf("stream body used the plain upstream stream instead of the orchestrator: %s", body)
	}
}
