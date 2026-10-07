//go:build e2e

package e2e_test

// Server-level e2e coverage for the executeChatUpstream image-handling paths
// (core_upstream.go): image-capable upstreams receive images unstripped and
// never touch the visual orchestrator, text-only upstreams get the strip +
// warn fall-through when visual assist is unavailable, and text-only requests
// stream directly as chunked SSE deltas even with the visual extension
// enabled (the needsAssist gate).

import (
	"bufio"
	"bytes"
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
	"providerbridge/internal/protocol/chat"
	"providerbridge/internal/service/provider"
	"providerbridge/internal/service/runtime"
	"providerbridge/internal/service/server"
)

// upstreamRecorder records raw request bodies a mock upstream received.
type upstreamRecorder struct {
	mu     sync.Mutex
	bodies []string
}

func (r *upstreamRecorder) add(body string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.bodies = append(r.bodies, body)
}

func (r *upstreamRecorder) count() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.bodies)
}

func (r *upstreamRecorder) lastBody() string {
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.bodies) == 0 {
		return ""
	}
	return r.bodies[len(r.bodies)-1]
}

// newChatUpstreamMock starts a mock OpenAI-chat upstream that records every
// request body and answers streaming requests with three distinct content
// deltas (so chunked streaming is observable on the wire) and non-streaming
// requests with a canned completion.
func newChatUpstreamMock(t *testing.T) (*httptest.Server, *upstreamRecorder) {
	t.Helper()

	rec := &upstreamRecorder{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		rec.add(string(body))

		var req struct {
			Model  string `json:"model"`
			Stream bool   `json:"stream"`
		}
		_ = json.Unmarshal(body, &req)

		if !req.Stream {
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]any{
				"id":      "chatcmpl-mock",
				"object":  "chat.completion",
				"created": 1,
				"model":   req.Model,
				"choices": []any{map[string]any{
					"index":         0,
					"message":       map[string]any{"role": "assistant", "content": "Hello stream"},
					"finish_reason": "stop",
				}},
				"usage": map[string]any{"prompt_tokens": 1, "completion_tokens": 2, "total_tokens": 3},
			})
			return
		}

		w.Header().Set("Content-Type", "text/event-stream")
		flusher, _ := w.(http.Flusher)
		for _, piece := range []string{"Hel", "lo st", "ream"} {
			fmt.Fprintf(w, "data: %s\n\n", mockStreamChunk(req.Model, piece, ""))
			if flusher != nil {
				flusher.Flush()
			}
		}
		fmt.Fprintf(w, "data: %s\n\n", mockStreamChunk(req.Model, "", "stop"))
		fmt.Fprintf(w, "data: %s\n\n", usageChunk(req.Model))
		fmt.Fprint(w, "data: [DONE]\n\n")
		if flusher != nil {
			flusher.Flush()
		}
	}))
	t.Cleanup(srv.Close)
	return srv, rec
}

// mockStreamChunk builds one chat.completion.chunk wire payload.
func mockStreamChunk(model, content, finish string) string {
	chunk := map[string]any{
		"id":      "chatcmpl-mock",
		"object":  "chat.completion.chunk",
		"created": 1,
		"model":   model,
		"choices": []any{map[string]any{
			"index":         0,
			"delta":         map[string]any{"content": content},
			"finish_reason": nil,
		}},
	}
	if finish != "" {
		chunk["choices"].([]any)[0].(map[string]any)["finish_reason"] = finish
	}
	data, _ := json.Marshal(chunk)
	return string(data)
}

// usageChunk builds the terminal usage-only chunk (empty choices).
func usageChunk(model string) string {
	return fmt.Sprintf(`{"id":"chatcmpl-mock","object":"chat.completion.chunk","created":1,"model":%q,"choices":[],"usage":{"prompt_tokens":1,"completion_tokens":2,"total_tokens":3}}`, model)
}

// chatVisualHarness wires a full Server against mock upstreams.
type chatVisualHarness struct {
	handler  http.Handler
	upstream *upstreamRecorder
	vision   *upstreamRecorder
}

// newChatVisualHarness builds the full server harness (config, provider
// manager, plugin registry with the visual plugin, runtime, adapter
// registry, chat clients) around two mock upstreams: the routed main
// upstream ("chat-main") and a vision provider ("vision-mock"). The main
// upstream model's input modalities and the visual extension are caller-
// controlled.
func newChatVisualHarness(t *testing.T, mainModalities []string, visualEnabled bool) *chatVisualHarness {
	t.Helper()
	if !visualEnabled {
		return newChatVisualHarnessWithVisual(t, mainModalities, "", "")
	}
	return newChatVisualHarnessWithVisual(t, mainModalities, "vision-mock", "vision-model")
}

// newChatVisualHarnessWithVisual builds the full harness with an explicit
// visual-extension provider/model pair. An empty visualProvider disables the
// extension; a provider key absent from ProviderDefs exercises the
// "configured but unresolvable" fall-through (wrapWithVisual returns nil).
func newChatVisualHarnessWithVisual(t *testing.T, mainModalities []string, visualProvider, visualModel string) *chatVisualHarness {
	t.Helper()

	upstreamSrv, upstreamRec := newChatUpstreamMock(t)
	visionSrv, visionRec := newChatUpstreamMock(t)

	route := config.RouteEntry{
		Provider:        "chat-main",
		Model:           "upstream-chat",
		InputModalities: mainModalities,
	}
	if visualProvider != "" && visualModel != "" {
		enabled := true
		route.Extensions = map[string]config.ExtensionSettings{
			"visual": {
				Enabled:   &enabled,
				RawConfig: map[string]any{"provider": visualProvider, "model": visualModel},
			},
		}
	}

	cfg := config.Config{
		Routes: map[string]config.RouteEntry{"chat-model": route},
		ProviderDefs: map[string]config.ProviderDef{
			"chat-main": {
				BaseURL:  upstreamSrv.URL,
				APIKey:   "e2e-key",
				Protocol: config.ProtocolOpenAIChat,
				Models: map[string]config.ModelMeta{
					"upstream-chat": {InputModalities: mainModalities},
				},
			},
			"vision-mock": {
				BaseURL:  visionSrv.URL,
				APIKey:   "e2e-key",
				Protocol: config.ProtocolOpenAIChat,
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
	chatClientAdapter := chat.NewChatClientAdapter(coreHooks)
	_ = adapterReg.RegisterClient(chatClientAdapter)
	_ = adapterReg.RegisterClientStream(chatClientAdapter)
	chatProviderAdapter := chat.NewChatProviderAdapter(2048, nil, coreHooks)
	_ = adapterReg.RegisterProvider(chatProviderAdapter)
	_ = adapterReg.RegisterProviderStream(chatProviderAdapter)

	chatClients := map[string]any{
		"chat-main":   chat.NewClient(chat.ClientConfig{BaseURL: upstreamSrv.URL, APIKey: "e2e-key"}),
		"vision-mock": chat.NewClient(chat.ClientConfig{BaseURL: visionSrv.URL, APIKey: "e2e-key"}),
	}

	serverCfg := config.ServerFromGlobalConfig(&cfg)
	handler := server.New(server.Config{
		ProviderMgr:     providerMgr,
		AdapterRegistry: adapterReg,
		ChatClients:     chatClients,
		AppConfig:       serverCfg,
		ServerCfg:       serverCfg,
		PluginRegistry:  pluginReg,
		Runtime:         runtime.NewRuntime(cfg, providerMgr, nil),
	})

	return &chatVisualHarness{handler: handler, upstream: upstreamRec, vision: visionRec}
}

// chatImageRequest is a streaming chat request carrying an image_url
// data-URL part.
func chatImageRequest() map[string]any {
	return map[string]any{
		"model":  "chat-model",
		"stream": true,
		"messages": []any{
			map[string]any{
				"role": "user",
				"content": []any{
					map[string]any{"type": "text", "text": "Describe the image."},
					map[string]any{"type": "image_url", "image_url": map[string]any{
						"url": "data:image/png;base64,AAAA",
					}},
				},
			},
		},
	}
}

// chatTextOnlyRequest is a streaming chat request without any image part.
func chatTextOnlyRequest() map[string]any {
	return map[string]any{
		"model":  "chat-model",
		"stream": true,
		"messages": []any{
			map[string]any{
				"role":    "user",
				"content": "Reply with the words Hello stream.",
			},
		},
	}
}

// postChatCompletionsStream POSTs the payload to the live server and reads
// the SSE response incrementally, returning the raw data payloads in
// arrival order.
func postChatCompletionsStream(t *testing.T, handler http.Handler, payload map[string]any) (int, []string) {
	t.Helper()

	body, err := json.Marshal(payload)
	if err != nil {
		t.Fatalf("marshal payload error = %v", err)
	}

	target := httptest.NewServer(handler)
	t.Cleanup(target.Close)

	resp, err := http.Post(target.URL+"/v1/chat/completions", "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatalf("post chat completions error = %v", err)
	}
	defer resp.Body.Close()

	var events []string
	scanner := bufio.NewScanner(resp.Body)
	scanner.Buffer(make([]byte, 64*1024), 4*1024*1024)
	for scanner.Scan() {
		line := scanner.Text()
		if line == "" {
			continue
		}
		if !strings.HasPrefix(line, "data: ") {
			continue
		}
		events = append(events, strings.TrimPrefix(line, "data: "))
	}
	return resp.StatusCode, events
}

// postChatCompletionsJSON POSTs the payload to the live server and returns the
// HTTP status and the raw JSON response body. It is the non-streaming sibling
// of postChatCompletionsStream and exercises executeChatUpstream's
// non-streaming branch.
func postChatCompletionsJSON(t *testing.T, handler http.Handler, payload map[string]any) (int, string) {
	t.Helper()

	body, err := json.Marshal(payload)
	if err != nil {
		t.Fatalf("marshal payload error = %v", err)
	}

	target := httptest.NewServer(handler)
	t.Cleanup(target.Close)

	resp, err := http.Post(target.URL+"/v1/chat/completions", "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatalf("post chat completions error = %v", err)
	}
	defer resp.Body.Close()

	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read chat completions body error = %v", err)
	}
	return resp.StatusCode, string(raw)
}

// chatStreamContentDelta extracts the content deltas from wire chunk events.
func chatStreamContentDeltas(events []string) []string {
	var deltas []string
	for _, event := range events {
		var chunk struct {
			Object  string `json:"object"`
			Choices []struct {
				Delta struct {
					Content string `json:"content"`
				} `json:"delta"`
			} `json:"choices"`
		}
		if err := json.Unmarshal([]byte(event), &chunk); err != nil {
			continue
		}
		if chunk.Object != "chat.completion.chunk" {
			continue
		}
		for _, choice := range chunk.Choices {
			if choice.Delta.Content != "" {
				deltas = append(deltas, choice.Delta.Content)
			}
		}
	}
	return deltas
}

// TestChatCompletions_ImageCapableModelReceivesImageUnstripped verifies that
// an image-capable (multimodal) upstream model receives the image_url data
// URL natively — no strip placeholder, no visual orchestrator round-trip.
func TestChatCompletions_ImageCapableModelReceivesImageUnstripped(t *testing.T) {
	h := newChatVisualHarness(t, []string{"text", "image"}, false)

	status, events := postChatCompletionsStream(t, h.handler, chatImageRequest())
	if status != http.StatusOK {
		t.Fatalf("status = %d, events = %v", status, events)
	}

	body := h.upstream.lastBody()
	if !strings.Contains(body, "image_url") {
		t.Fatalf("upstream body missing image_url part: %s", body)
	}
	if !strings.Contains(body, "data:image/png;base64,AAAA") {
		t.Fatalf("upstream body missing the image data URL: %s", body)
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

// TestChatCompletions_TextOnlyModelStripsImagesOnFallThrough verifies that a
// text-only upstream model with visual assist unavailable (extension not
// enabled, so wrapWithVisual returns nil) falls through to the strip path:
// the image is replaced by the placeholder text and a warning is emitted.
func TestChatCompletions_TextOnlyModelStripsImagesOnFallThrough(t *testing.T) {
	h := newChatVisualHarness(t, []string{"text"}, false)

	status, events := postChatCompletionsStream(t, h.handler, chatImageRequest())
	if status != http.StatusOK {
		t.Fatalf("status = %d, events = %v", status, events)
	}

	body := h.upstream.lastBody()
	if !strings.Contains(body, "Image #1 is available to Visual Brief and Visual QA") {
		t.Fatalf("upstream body missing the visual strip placeholder: %s", body)
	}
	if strings.Contains(body, "data:image/png;base64,AAAA") {
		t.Fatalf("upstream body still contains the image data URL: %s", body)
	}
	if h.upstream.count() != 1 {
		t.Fatalf("upstream call count = %d, want 1", h.upstream.count())
	}
	if got := h.vision.count(); got != 0 {
		t.Fatalf("vision provider call count = %d, want 0 (no orchestrator)", got)
	}
}

// TestChatCompletionsNonStreaming_ImageCapableModelReceivesImageUnstripped
// covers executeChatUpstream's NON-streaming branch for an image-capable
// upstream: the image_url data URL must reach the upstream and the visual
// orchestrator must not run.
func TestChatCompletionsNonStreaming_ImageCapableModelReceivesImageUnstripped(t *testing.T) {
	h := newChatVisualHarness(t, []string{"text", "image"}, false)

	payload := chatImageRequest()
	payload["stream"] = false
	status, raw := postChatCompletionsJSON(t, h.handler, payload)
	if status != http.StatusOK {
		t.Fatalf("status = %d, body = %s", status, raw)
	}

	body := h.upstream.lastBody()
	if !strings.Contains(body, "data:image/png;base64,AAAA") {
		t.Fatalf("upstream body missing the image data URL: %s", body)
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

// TestChatCompletionsNonStreaming_TextOnlyModelStripsImagesOnFallThrough
// covers executeChatUpstream's NON-streaming strip fall-through: a text-only
// upstream with visual assist unavailable receives the placeholder, not the
// image, and the client still gets a normal completion.
func TestChatCompletionsNonStreaming_TextOnlyModelStripsImagesOnFallThrough(t *testing.T) {
	h := newChatVisualHarness(t, []string{"text"}, false)

	payload := chatImageRequest()
	payload["stream"] = false
	status, raw := postChatCompletionsJSON(t, h.handler, payload)
	if status != http.StatusOK {
		t.Fatalf("status = %d, body = %s", status, raw)
	}
	if !strings.Contains(raw, "Hello stream") {
		t.Fatalf("client response missing upstream completion: %s", raw)
	}

	body := h.upstream.lastBody()
	if !strings.Contains(body, "Image #1 is available to Visual Brief and Visual QA") {
		t.Fatalf("upstream body missing the visual strip placeholder: %s", body)
	}
	if strings.Contains(body, "data:image/png;base64,AAAA") {
		t.Fatalf("upstream body still contains the image data URL: %s", body)
	}
	if h.upstream.count() != 1 {
		t.Fatalf("upstream call count = %d, want 1", h.upstream.count())
	}
	if got := h.vision.count(); got != 0 {
		t.Fatalf("vision provider call count = %d, want 0 (no orchestrator)", got)
	}
}

// TestChatCompletions_TextOnlyRequestStreamsDirectly verifies that a request
// without images streams directly to the upstream even when the visual
// extension is enabled for the model: exactly one upstream streaming call
// and true chunked SSE deltas in the client's wire shape (no synthesized
// single-buffer response).
func TestChatCompletions_TextOnlyRequestStreamsDirectly(t *testing.T) {
	h := newChatVisualHarness(t, []string{"text"}, true)

	status, events := postChatCompletionsStream(t, h.handler, chatTextOnlyRequest())
	if status != http.StatusOK {
		t.Fatalf("status = %d, events = %v", status, events)
	}
	if h.upstream.count() != 1 {
		t.Fatalf("upstream call count = %d, want 1 (single direct streaming call)", h.upstream.count())
	}
	if got := h.vision.count(); got != 0 {
		t.Fatalf("vision provider call count = %d, want 0 (no orchestrator)", got)
	}

	deltas := chatStreamContentDeltas(events)
	if len(deltas) < 2 {
		t.Fatalf("content deltas = %v, want at least 2 chunked deltas", deltas)
	}
	joined := strings.Join(deltas, "")
	if joined != "Hello stream" {
		t.Fatalf("joined deltas = %q, want %q", joined, "Hello stream")
	}
}

// TestChatCompletions_UnresolvableVisualProviderStripsImagesWithWarning
// covers the configured-but-unresolvable visual provider fall-through: the
// visual extension is enabled for the model, but its configured provider key
// resolves to no protocol, so wrapWithVisual returns nil, the image is
// replaced by the strip placeholder, and the explicit English warning is
// logged before forwarding to the text-only upstream.
func TestChatCompletions_UnresolvableVisualProviderStripsImagesWithWarning(t *testing.T) {
	var logs bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{Level: slog.LevelWarn})))
	defer slog.SetDefault(prev)

	h := newChatVisualHarnessWithVisual(t, []string{"text"}, "vision-ghost", "vision-model")

	status, events := postChatCompletionsStream(t, h.handler, chatImageRequest())
	if status != http.StatusOK {
		t.Fatalf("status = %d, events = %v", status, events)
	}

	body := h.upstream.lastBody()
	if !strings.Contains(body, "Image #1 is available to Visual Brief and Visual QA") {
		t.Fatalf("upstream body missing the visual strip placeholder: %s", body)
	}
	if strings.Contains(body, "data:image/png;base64,AAAA") {
		t.Fatalf("upstream body still contains the image data URL: %s", body)
	}
	if h.upstream.count() != 1 {
		t.Fatalf("upstream call count = %d, want 1", h.upstream.count())
	}
	if got := h.vision.count(); got != 0 {
		t.Fatalf("vision provider call count = %d, want 0 (unresolvable provider)", got)
	}
	if !strings.Contains(logs.String(), "visual assist unavailable") {
		t.Fatalf("expected the visual-assist warning to be logged, got: %s", logs.String())
	}
}
