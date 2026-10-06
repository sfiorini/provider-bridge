//go:build e2e

package e2e_test

// Server-level e2e coverage for the executeAnthropicUpstream image-handling
// paths (core_upstream.go): image-capable upstreams receive images unstripped
// and never touch the visual orchestrator; text-only upstreams get the
// orchestrator when it is available, or the strip + warn fall-through when it
// is not. Both the non-streaming and streaming inbound shapes are covered.

import (
	"bufio"
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

	"providerbridge/internal/config"
	"providerbridge/internal/extension/plugin"
	"providerbridge/internal/extension/visual"
	"providerbridge/internal/format"
	"providerbridge/internal/protocol/anthropic"
	"providerbridge/internal/service/provider"
	"providerbridge/internal/service/runtime"
	"providerbridge/internal/service/server"
)

// anthropicUpstreamMock is a mock Anthropic Messages upstream that records
// every request body. Non-streaming calls answer with a visual_brief tool_use
// on the first call when firstToolUse is set (so the orchestrator calls the
// vision provider) and a plain text message otherwise; streaming calls answer
// with a valid Anthropic SSE sequence.
type anthropicUpstreamMock struct {
	mu           sync.Mutex
	bodies       []string
	calls        int
	firstToolUse bool
}

func (m *anthropicUpstreamMock) add(body string) int {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.bodies = append(m.bodies, body)
	m.calls++
	return m.calls
}

func (m *anthropicUpstreamMock) count() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.bodies)
}

func (m *anthropicUpstreamMock) lastBody() string {
	m.mu.Lock()
	defer m.mu.Unlock()
	if len(m.bodies) == 0 {
		return ""
	}
	return m.bodies[len(m.bodies)-1]
}

func (m *anthropicUpstreamMock) firstBody() string {
	m.mu.Lock()
	defer m.mu.Unlock()
	if len(m.bodies) == 0 {
		return ""
	}
	return m.bodies[0]
}

// anthropicTextResponse builds a non-streaming text MessageResponse.
func anthropicTextResponse(text string) string {
	payload := map[string]any{
		"id":          "msg_mock_text",
		"type":        "message",
		"role":        "assistant",
		"content":     []any{map[string]any{"type": "text", "text": text}},
		"model":       "mock-model",
		"stop_reason": "end_turn",
		"usage":       map[string]any{"input_tokens": 10, "output_tokens": 5},
	}
	data, _ := json.Marshal(payload)
	return string(data)
}

// anthropicToolUseResponse builds a non-streaming visual_brief tool_use
// response, which drives the visual orchestrator to call the vision provider.
func anthropicToolUseResponse() string {
	payload := map[string]any{
		"id":   "msg_mock_tool",
		"type": "message",
		"role": "assistant",
		"content": []any{map[string]any{
			"type":  "tool_use",
			"id":    "call_visual_1",
			"name":  "visual_brief",
			"input": map[string]any{"image_refs": []any{"Image #1"}, "context": "describe"},
		}},
		"model":       "mock-model",
		"stop_reason": "tool_use",
		"usage":       map[string]any{"input_tokens": 10, "output_tokens": 5},
	}
	data, _ := json.Marshal(payload)
	return string(data)
}

// newAnthropicMock starts a mock upstream and wires the handler above.
func newAnthropicMock(t *testing.T, firstToolUse bool) (*httptest.Server, *anthropicUpstreamMock) {
	t.Helper()

	mock := &anthropicUpstreamMock{firstToolUse: firstToolUse}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		call := mock.add(string(body))

		if strings.Contains(string(body), `"stream":true`) {
			writeAnthropicSSE(w, "Hello from streaming mock!")
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		if mock.firstToolUse && call == 1 {
			fmt.Fprint(w, anthropicToolUseResponse())
			return
		}
		fmt.Fprint(w, anthropicTextResponse("Hello from Anthropic mock!"))
	}))
	t.Cleanup(srv.Close)
	return srv, mock
}

// writeAnthropicSSE writes a minimal but valid Anthropic SSE sequence.
func writeAnthropicSSE(w http.ResponseWriter, text string) {
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.WriteHeader(http.StatusOK)
	flusher, _ := w.(http.Flusher)

	emit := func(event, data string) {
		fmt.Fprintf(w, "event: %s\ndata: %s\n\n", event, data)
		if flusher != nil {
			flusher.Flush()
		}
	}
	emit("message_start", `{"type":"message_start","message":{"id":"msg_str_mock","type":"message","role":"assistant","content":[],"model":"mock-model","usage":{"input_tokens":5,"output_tokens":0}}}`)
	emit("content_block_start", `{"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`)
	emit("content_block_delta", fmt.Sprintf(`{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":%q}}`, text))
	emit("content_block_stop", `{"type":"content_block_stop","index":0}`)
	emit("message_delta", `{"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"input_tokens":5,"output_tokens":3}}`)
	emit("message_stop", `{"type":"message_stop"}`)
}

// anthropicVisualHarness wires a full Server against mock Anthropic upstreams.
type anthropicVisualHarness struct {
	handler  http.Handler
	upstream *anthropicUpstreamMock
	vision   *anthropicUpstreamMock
}

// newAnthropicVisualHarness builds the full server harness around two mock
// Anthropic upstreams: the routed main upstream ("anthropic-main") and a
// vision provider ("anthropic-vision"). The main upstream model's input
// modalities and the visual extension are caller-controlled. When
// firstToolUse is set the main upstream asks for a visual_brief on its first
// call so the orchestrator exercises the vision provider.
func newAnthropicVisualHarness(t *testing.T, mainModalities []string, visualEnabled, firstToolUse bool) *anthropicVisualHarness {
	t.Helper()

	upstreamSrv, upstreamMock := newAnthropicMock(t, firstToolUse)
	visionSrv, visionMock := newAnthropicMock(t, false)

	route := config.RouteEntry{
		Provider:        "anthropic-main",
		Model:           "upstream-anthropic",
		InputModalities: mainModalities,
	}
	if visualEnabled {
		enabled := true
		route.Extensions = map[string]config.ExtensionSettings{
			"visual": {
				Enabled:   &enabled,
				RawConfig: map[string]any{"provider": "anthropic-vision", "model": "vision-model"},
			},
		}
	}

	cfg := config.Config{
		Routes: map[string]config.RouteEntry{"anthropic-model": route},
		ProviderDefs: map[string]config.ProviderDef{
			"anthropic-main": {
				BaseURL:  upstreamSrv.URL,
				APIKey:   "e2e-key",
				Protocol: config.ProtocolAnthropic,
				Models: map[string]config.ModelMeta{
					"upstream-anthropic": {InputModalities: mainModalities},
				},
			},
			"anthropic-vision": {
				BaseURL:  visionSrv.URL,
				APIKey:   "e2e-key",
				Protocol: config.ProtocolAnthropic,
				Models: map[string]config.ModelMeta{
					"vision-model": {InputModalities: []string{"text", "image"}},
				},
			},
		},
	}

	providerMgr, err := provider.NewProviderManager(buildProviderDefsForTest(cfg), buildModelRoutesForTest(cfg))
	if err != nil {
		t.Fatalf("failed to create provider manager: %v", err)
	}

	pluginReg := plugin.NewRegistry(slog.Default())
	pluginReg.Register(visual.NewPlugin())
	pluginReg.SetCurrentConfigProvider(func() config.Config { return cfg })
	if err := pluginReg.InitAll(&cfg); err != nil {
		t.Fatalf("failed to init plugins: %v", err)
	}

	coreHooks := pluginReg.CorePluginHooks()
	adapterReg := format.NewRegistry()
	anthClientAdapter := anthropic.NewAnthropicClientAdapter(coreHooks)
	_ = adapterReg.RegisterClient(anthClientAdapter)
	_ = adapterReg.RegisterClientStream(anthClientAdapter)
	anthProviderAdapter := anthropic.NewAnthropicProviderAdapter(2048, noopCacheManagerForTest{}, coreHooks)
	_ = adapterReg.RegisterProvider(anthProviderAdapter)
	_ = adapterReg.RegisterProviderStream(anthProviderAdapter)

	serverCfg := config.ServerFromGlobalConfig(&cfg)
	handler := server.New(server.Config{
		ProviderMgr:     providerMgr,
		AdapterRegistry: adapterReg,
		AppConfig:       serverCfg,
		ServerCfg:       serverCfg,
		PluginRegistry:  pluginReg,
		Runtime:         runtime.NewRuntime(cfg, providerMgr, nil),
	})

	return &anthropicVisualHarness{handler: handler, upstream: upstreamMock, vision: visionMock}
}

// noopCacheManagerForTest satisfies anthropic.CacheManager.
type noopCacheManagerForTest struct{}

func (noopCacheManagerForTest) PlanAndInject(_ context.Context, _ *anthropic.MessageRequest, _ *format.CoreRequest) (string, string) {
	return "", ""
}

func (noopCacheManagerForTest) UpdateRegistry(_ context.Context, _, _ string, _ anthropic.Usage) {}

// anthropicImageRequest is an Anthropic Messages request carrying an image
// content block.
func anthropicImageRequest(stream bool) map[string]any {
	return map[string]any{
		"model":      "anthropic-model",
		"max_tokens": 128,
		"stream":     stream,
		"messages": []any{
			map[string]any{
				"role": "user",
				"content": []any{
					map[string]any{"type": "text", "text": "Describe the image."},
					map[string]any{"type": "image", "source": map[string]any{
						"type":       "base64",
						"media_type": "image/png",
						"data":       "AAAA",
					}},
				},
			},
		},
	}
}

// postAnthropicMessagesJSON posts a non-streaming request and returns status
// plus the raw response body.
func postAnthropicMessagesJSON(t *testing.T, handler http.Handler, payload map[string]any) (int, string) {
	t.Helper()

	body, err := json.Marshal(payload)
	if err != nil {
		t.Fatalf("marshal payload error = %v", err)
	}
	target := httptest.NewServer(handler)
	t.Cleanup(target.Close)

	resp, err := http.Post(target.URL+"/v1/messages", "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatalf("post messages error = %v", err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read messages body error = %v", err)
	}
	return resp.StatusCode, string(raw)
}

// postAnthropicMessagesSSE posts a streaming request and returns status plus
// the collected raw SSE lines.
func postAnthropicMessagesSSE(t *testing.T, handler http.Handler, payload map[string]any) (int, string) {
	t.Helper()

	body, err := json.Marshal(payload)
	if err != nil {
		t.Fatalf("marshal payload error = %v", err)
	}
	target := httptest.NewServer(handler)
	t.Cleanup(target.Close)

	resp, err := http.Post(target.URL+"/v1/messages", "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatalf("post messages error = %v", err)
	}
	defer resp.Body.Close()

	var sb strings.Builder
	scanner := bufio.NewScanner(resp.Body)
	scanner.Buffer(make([]byte, 64*1024), 4*1024*1024)
	for scanner.Scan() {
		sb.WriteString(scanner.Text())
		sb.WriteString("\n")
	}
	return resp.StatusCode, sb.String()
}

// TestAnthropicMessagesNonStreaming_ImageCapableModelReceivesImageUnstripped
// covers executeAnthropicUpstream's NON-streaming branch for an image-capable
// upstream.
func TestAnthropicMessagesNonStreaming_ImageCapableModelReceivesImageUnstripped(t *testing.T) {
	h := newAnthropicVisualHarness(t, []string{"text", "image"}, false, false)

	status, raw := postAnthropicMessagesJSON(t, h.handler, anthropicImageRequest(false))
	if status != http.StatusOK {
		t.Fatalf("status = %d, body = %s", status, raw)
	}

	body := h.upstream.lastBody()
	if !strings.Contains(body, `"type":"image"`) || !strings.Contains(body, `"AAAA"`) {
		t.Fatalf("upstream body missing the image block: %s", body)
	}
	if strings.Contains(body, "Image #1 is available to Visual Brief and Visual QA") {
		t.Fatalf("upstream body contains the visual strip placeholder: %s", body)
	}
	if h.upstream.count() != 1 {
		t.Fatalf("upstream call count = %d, want 1", h.upstream.count())
	}
	if got := h.vision.count(); got != 0 {
		t.Fatalf("vision provider call count = %d, want 0 (no orchestrator)", got)
	}
}

// TestAnthropicMessagesNonStreaming_TextOnlyModelStripsImagesOnFallThrough
// covers executeAnthropicUpstream's NON-streaming strip fall-through (visual
// extension disabled, so wrapWithVisual returns nil).
func TestAnthropicMessagesNonStreaming_TextOnlyModelStripsImagesOnFallThrough(t *testing.T) {
	h := newAnthropicVisualHarness(t, []string{"text"}, false, false)

	status, raw := postAnthropicMessagesJSON(t, h.handler, anthropicImageRequest(false))
	if status != http.StatusOK {
		t.Fatalf("status = %d, body = %s", status, raw)
	}
	if !strings.Contains(raw, "Hello from Anthropic mock!") {
		t.Fatalf("client response missing upstream completion: %s", raw)
	}

	body := h.upstream.lastBody()
	if !strings.Contains(body, "Image #1 is available to Visual Brief and Visual QA") {
		t.Fatalf("upstream body missing the visual strip placeholder: %s", body)
	}
	if strings.Contains(body, `"AAAA"`) {
		t.Fatalf("upstream body still contains the raw image data: %s", body)
	}
	if h.upstream.count() != 1 {
		t.Fatalf("upstream call count = %d, want 1", h.upstream.count())
	}
}

// TestAnthropicMessagesStreaming_ImageCapableModelReceivesImageUnstripped
// covers executeAnthropicUpstream's streaming branch for an image-capable
// upstream.
func TestAnthropicMessagesStreaming_ImageCapableModelReceivesImageUnstripped(t *testing.T) {
	h := newAnthropicVisualHarness(t, []string{"text", "image"}, false, false)

	status, raw := postAnthropicMessagesSSE(t, h.handler, anthropicImageRequest(true))
	if status != http.StatusOK {
		t.Fatalf("status = %d, body = %s", status, raw)
	}
	if !strings.Contains(raw, "Hello from streaming mock!") {
		t.Fatalf("client stream missing upstream text: %s", raw)
	}

	body := h.upstream.lastBody()
	if !strings.Contains(body, `"type":"image"`) || !strings.Contains(body, `"AAAA"`) {
		t.Fatalf("upstream body missing the image block: %s", body)
	}
	if h.upstream.count() != 1 {
		t.Fatalf("upstream call count = %d, want 1", h.upstream.count())
	}
	if got := h.vision.count(); got != 0 {
		t.Fatalf("vision provider call count = %d, want 0 (no orchestrator)", got)
	}
}

// TestAnthropicMessagesStreaming_TextOnlyModelStripsImagesOnFallThrough
// covers executeAnthropicUpstream's streaming strip fall-through.
func TestAnthropicMessagesStreaming_TextOnlyModelStripsImagesOnFallThrough(t *testing.T) {
	h := newAnthropicVisualHarness(t, []string{"text"}, false, false)

	status, raw := postAnthropicMessagesSSE(t, h.handler, anthropicImageRequest(true))
	if status != http.StatusOK {
		t.Fatalf("status = %d, body = %s", status, raw)
	}
	if !strings.Contains(raw, "Hello from streaming mock!") {
		t.Fatalf("client stream missing upstream text: %s", raw)
	}

	body := h.upstream.lastBody()
	if !strings.Contains(body, "Image #1 is available to Visual Brief and Visual QA") {
		t.Fatalf("upstream body missing the visual strip placeholder: %s", body)
	}
	if strings.Contains(body, `"AAAA"`) {
		t.Fatalf("upstream body still contains the raw image data: %s", body)
	}
}

// TestAnthropicMessagesNonStreaming_VisualOrchestratorRunsWhenNeedsAssist
// proves the orchestrator path is taken (not the strip) when visual assist is
// available: the main upstream sees the placeholder, the vision provider is
// called, and the client receives the final answer.
func TestAnthropicMessagesNonStreaming_VisualOrchestratorRunsWhenNeedsAssist(t *testing.T) {
	h := newAnthropicVisualHarness(t, []string{"text"}, true, true)

	status, raw := postAnthropicMessagesJSON(t, h.handler, anthropicImageRequest(false))
	if status != http.StatusOK {
		t.Fatalf("status = %d, body = %s", status, raw)
	}
	if !strings.Contains(raw, "Hello from Anthropic mock!") {
		t.Fatalf("client response missing upstream final answer: %s", raw)
	}

	first := h.upstream.firstBody()
	if !strings.Contains(first, "Image #1 is available to Visual Brief and Visual QA") {
		t.Fatalf("orchestrator upstream body missing the visual placeholder: %s", first)
	}
	if strings.Contains(first, `"AAAA"`) {
		t.Fatalf("orchestrator upstream body still contains the raw image data: %s", first)
	}
	if h.upstream.count() < 2 {
		t.Fatalf("upstream call count = %d, want at least 2 (tool_use round + final)", h.upstream.count())
	}
	if got := h.vision.count(); got != 1 {
		t.Fatalf("vision provider call count = %d, want 1", got)
	}
	visBody := h.vision.lastBody()
	if !strings.Contains(visBody, `"AAAA"`) {
		t.Fatalf("vision provider body missing the forwarded image: %s", visBody)
	}
}

// TestAnthropicMessagesStreaming_VisualOrchestratorRunsWhenNeedsAssist is the
// streaming counterpart: the orchestrator runs non-streaming and the final
// answer is synthesized into the client SSE stream.
func TestAnthropicMessagesStreaming_VisualOrchestratorRunsWhenNeedsAssist(t *testing.T) {
	h := newAnthropicVisualHarness(t, []string{"text"}, true, true)

	status, raw := postAnthropicMessagesSSE(t, h.handler, anthropicImageRequest(true))
	if status != http.StatusOK {
		t.Fatalf("status = %d, body = %s", status, raw)
	}
	if !strings.Contains(raw, "Hello from Anthropic mock!") {
		t.Fatalf("client stream missing synthesized final answer: %s", raw)
	}

	first := h.upstream.firstBody()
	if !strings.Contains(first, "Image #1 is available to Visual Brief and Visual QA") {
		t.Fatalf("orchestrator upstream body missing the visual placeholder: %s", first)
	}
	if got := h.vision.count(); got != 1 {
		t.Fatalf("vision provider call count = %d, want 1", got)
	}
}
