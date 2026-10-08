// Package server tests for API-key rotation: the per-index chat client
// cache and the active chat caller selection.
package server

import (
	"bytes"
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"slices"
	"sync"
	"testing"

	"providerbridge/internal/config"
	"providerbridge/internal/format"
	"providerbridge/internal/protocol/anthropic"
	"providerbridge/internal/protocol/chat"
	"providerbridge/internal/protocol/google"
	"providerbridge/internal/service/provider"
	"providerbridge/internal/service/runtime"
)

// fakeRotationStore is an in-memory provider.KeyRotationStore recording
// persist calls (thread-safe; persist is asynchronous).
type fakeRotationStore struct {
	mu    sync.Mutex
	idx   map[string]int
	calls []string
}

func (f *fakeRotationStore) LoadProviderKeyIndexes(_ context.Context) (map[string]int, error) {
	return f.idx, nil
}

func (f *fakeRotationStore) SetProviderKeyIndex(_ context.Context, providerKey string, idx int) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, fmt.Sprintf("%s=%d", providerKey, idx))
	return nil
}

// newKeyRotationServer builds a Server whose runtime snapshot has an
// openai-chat provider with a multi-key api_key, and whose provider manager
// parses the same keys.
func newKeyRotationServer(t *testing.T, pm *provider.ProviderManager, cfg config.Config) *Server {
	t.Helper()
	return New(Config{
		ProviderMgr: pm,
		Runtime:     runtime.NewRuntime(cfg, pm, nil),
	})
}

// TestActiveChatClientUsesActiveKey checks that the runtime-driven chat
// client cache builds plain clients with the ACTIVE API key (rotation
// index), never the raw comma-separated key list.
func TestActiveChatClientUsesActiveKey(t *testing.T) {
	var mu sync.Mutex
	var auth string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		auth = r.Header.Get("Authorization")
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"id":"chatcmpl-1","object":"chat.completion","created":1,"model":"m","choices":[{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}]}`)
	}))
	defer upstream.Close()

	pm, err := provider.NewProviderManager(
		map[string]provider.ProviderConfig{
			"main": {
				BaseURL:    upstream.URL,
				APIKey:     "k1,k2",
				Protocol:   config.ProtocolOpenAIChat,
				ModelNames: []string{"m"},
			},
		},
		nil,
	)
	if err != nil {
		t.Fatalf("NewProviderManager() error = %v", err)
	}
	// Force the active rotation index to 1 via the persisted-index load.
	pm.SetKeyRotationStore(&fakeRotationStore{idx: map[string]int{"main": 1}})

	cfg := config.Config{
		Mode: config.ModeTransform,
		ProviderDefs: map[string]config.ProviderDef{
			"main": {
				BaseURL:  upstream.URL,
				APIKey:   "k1,k2",
				Protocol: config.ProtocolOpenAIChat,
				Models:   map[string]config.ModelMeta{"m": {}},
			},
		},
	}
	srv := newKeyRotationServer(t, pm, cfg)

	raw := srv.activeChatClient("main")
	client, ok := raw.(*chat.Client)
	if !ok {
		t.Fatalf("activeChatClient(main) = %T, want *chat.Client", raw)
	}
	req := &chat.ChatRequest{Model: "m", Messages: []chat.ChatMessage{{Role: "user", Content: "hi"}}}
	if _, err := client.CreateChat(context.Background(), req); err != nil {
		t.Fatalf("CreateChat() error = %v", err)
	}
	mu.Lock()
	got := auth
	mu.Unlock()
	if got != "Bearer k2" {
		t.Fatalf("upstream Authorization = %q, want %q (the ACTIVE key)", got, "Bearer k2")
	}
}

// newMultiKeyChatServer returns a Server wired for the multi-key chat
// provider "main" (api_key "k1,k2") with both the manager and the runtime
// snapshot config.
func newMultiKeyChatServer(t *testing.T) *Server {
	t.Helper()
	pm, err := provider.NewProviderManager(
		map[string]provider.ProviderConfig{
			"main": {
				BaseURL:    "http://upstream.test",
				APIKey:     "k1,k2",
				Protocol:   config.ProtocolOpenAIChat,
				ModelNames: []string{"m"},
			},
		},
		nil,
	)
	if err != nil {
		t.Fatalf("NewProviderManager() error = %v", err)
	}
	cfg := config.Config{
		Mode: config.ModeTransform,
		ProviderDefs: map[string]config.ProviderDef{
			"main": {
				BaseURL:  "http://upstream.test",
				APIKey:   "k1,k2",
				Protocol: config.ProtocolOpenAIChat,
				Models:   map[string]config.ModelMeta{"m": {}},
			},
		},
	}
	return newKeyRotationServer(t, pm, cfg)
}

// TestChatClientIndexCache checks that chatClientIndex builds one client per
// (provider, index) and returns pointer-stable cached instances.
func TestChatClientIndexCache(t *testing.T) {
	srv := newMultiKeyChatServer(t)

	c0a := srv.chatClientIndex("main", 0)
	c1a := srv.chatClientIndex("main", 1)
	if c0a == nil || c1a == nil {
		t.Fatalf("chatClientIndex(main, 0/1) = %v/%v, want both non-nil", c0a, c1a)
	}
	if c0a == c1a {
		t.Fatal("chatClientIndex(main, 0) == chatClientIndex(main, 1), want distinct clients per index")
	}
	if c0b := srv.chatClientIndex("main", 0); c0b != c0a {
		t.Fatal("chatClientIndex(main, 0) is not pointer-stable across calls")
	}
	if c1b := srv.chatClientIndex("main", 1); c1b != c1a {
		t.Fatal("chatClientIndex(main, 1) is not pointer-stable across calls")
	}
}

// TestActiveChatCallerRotatingMultiKey checks that a multi-key provider gets
// a rotating ChatCaller.
func TestActiveChatCallerRotatingMultiKey(t *testing.T) {
	srv := newMultiKeyChatServer(t)

	if n := srv.activeProviderManager().ProviderKeyCount("main"); n != 2 {
		t.Fatalf("ProviderKeyCount(main) = %d, want 2", n)
	}
	first := srv.activeChatCaller("main")
	if first == nil {
		t.Fatal("activeChatCaller(main) = nil, want a rotating ChatCaller")
	}
	var _ provider.ChatCaller = first
	second := srv.activeChatCaller("main")
	if second == nil {
		t.Fatal("activeChatCaller(main) second call = nil, want a rotating ChatCaller")
	}
}

// TestActiveChatCallerSingleKeyPlain checks that a single-key provider still
// gets the injected plain chat client (no rotation machinery).
func TestActiveChatCallerSingleKeyPlain(t *testing.T) {
	pm, err := provider.NewProviderManager(
		map[string]provider.ProviderConfig{
			"main": {
				BaseURL:    "http://upstream.test",
				APIKey:     "k",
				Protocol:   config.ProtocolOpenAIChat,
				ModelNames: []string{"m"},
			},
		},
		nil,
	)
	if err != nil {
		t.Fatalf("NewProviderManager() error = %v", err)
	}
	injected := chat.NewClient(chat.ClientConfig{BaseURL: "http://injected.test", APIKey: "k"})
	srv := New(Config{
		ProviderMgr: pm,
		ChatClients: map[string]any{"main": injected},
	})

	caller := srv.activeChatCaller("main")
	if caller == nil {
		t.Fatal("activeChatCaller(main) = nil, want the injected plain client")
	}
	plain, ok := caller.(*chat.Client)
	if !ok {
		t.Fatalf("activeChatCaller(main) = %T, want *chat.Client (plain, no rotation)", caller)
	}
	if plain != injected {
		t.Fatal("activeChatCaller(main) returned a different client, want the injected one")
	}
}

// TestChatClientCacheInvalidatedOnManagerReload checks that the lazily-built
// chat client caches are cleared when the active provider manager changes
// (runtime Reload): a cached client built with the OLD key must never serve
// requests after the runtime reloaded with a NEW key.
func TestChatClientCacheInvalidatedOnManagerReload(t *testing.T) {
	var mu sync.Mutex
	var auth string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		auth = r.Header.Get("Authorization")
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"id":"chatcmpl-1","object":"chat.completion","created":1,"model":"m","choices":[{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}]}`)
	}))
	defer upstream.Close()

	newCfg := func(apiKey string) config.Config {
		return config.Config{
			Mode: config.ModeTransform,
			ProviderDefs: map[string]config.ProviderDef{
				"main": {
					BaseURL:  upstream.URL,
					APIKey:   apiKey,
					Protocol: config.ProtocolOpenAIChat,
					Models:   map[string]config.ModelMeta{"m": {}},
				},
			},
		}
	}

	cfg := newCfg("k1")
	pm, err := provider.NewProviderManager(
		map[string]provider.ProviderConfig{
			"main": {
				BaseURL:    upstream.URL,
				APIKey:     "k1",
				Protocol:   config.ProtocolOpenAIChat,
				ModelNames: []string{"m"},
			},
		},
		nil,
	)
	if err != nil {
		t.Fatalf("NewProviderManager() error = %v", err)
	}
	rt := runtime.NewRuntime(cfg, pm, nil)
	srv := New(Config{ProviderMgr: pm, Runtime: rt})

	// Prime both caches: the plain-key cache and the per-index cache.
	first := srv.activeChatClient("main")
	if first == nil {
		t.Fatal("activeChatClient(main) = nil before reload")
	}
	firstIdx := srv.chatClientIndex("main", 0)
	if firstIdx == nil {
		t.Fatal("chatClientIndex(main, 0) = nil before reload")
	}

	// Simulate a runtime reload with a different API key: the runtime swaps
	// in a NEW provider manager.
	if err := rt.Reload(newCfg("k2")); err != nil {
		t.Fatalf("Reload() error = %v", err)
	}

	// The plain-key cache must have been invalidated: the new client's
	// requests carry the NEW key.
	second := srv.activeChatClient("main")
	client, ok := second.(*chat.Client)
	if !ok {
		t.Fatalf("activeChatClient(main) after reload = %T, want *chat.Client", second)
	}
	req := &chat.ChatRequest{Model: "m", Messages: []chat.ChatMessage{{Role: "user", Content: "hi"}}}
	if _, err := client.CreateChat(context.Background(), req); err != nil {
		t.Fatalf("CreateChat() error = %v", err)
	}
	mu.Lock()
	got := auth
	mu.Unlock()
	if got != "Bearer k2" {
		t.Fatalf("upstream Authorization = %q, want %q (post-reload key)", got, "Bearer k2")
	}

	// The per-index cache must have been invalidated too.
	secondIdx := srv.chatClientIndex("main", 0)
	if secondIdx == nil {
		t.Fatal("chatClientIndex(main, 0) = nil after reload")
	}
	if secondIdx == firstIdx {
		t.Fatal("chatClientIndex(main, 0) after reload returned the pre-reload cached client")
	}
	if _, err := secondIdx.CreateChat(context.Background(), req); err != nil {
		t.Fatalf("CreateChat() (per-index) error = %v", err)
	}
	mu.Lock()
	got = auth
	mu.Unlock()
	if got != "Bearer k2" {
		t.Fatalf("upstream Authorization (per-index) = %q, want %q (post-reload key)", got, "Bearer k2")
	}
}

// TestChatClientsUseManagerHTTPClient checks that per-index and plain chat
// clients are built with the manager's proxy-aware *http.Client
// (ClientOverride), so their requests go through the configured egress
// proxy instead of http.DefaultClient.
func TestChatClientsUseManagerHTTPClient(t *testing.T) {
	var proxyMu sync.Mutex
	proxyHits := 0
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		proxyMu.Lock()
		proxyHits++
		proxyMu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"id":"chatcmpl-1","object":"chat.completion","created":1,"model":"m","choices":[{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}]}`)
	}))
	defer proxy.Close()
	proxyURL, err := url.Parse(proxy.URL)
	if err != nil {
		t.Fatalf("Parse(proxy URL) error = %v", err)
	}
	override := &http.Client{Transport: &http.Transport{Proxy: http.ProxyURL(proxyURL)}}

	pm, err := provider.NewProviderManager(
		map[string]provider.ProviderConfig{
			"main": {
				BaseURL:        "http://upstream.test",
				APIKey:         "k1,k2",
				Protocol:       config.ProtocolOpenAIChat,
				ModelNames:     []string{"m"},
				ClientOverride: override,
			},
		},
		nil,
	)
	if err != nil {
		t.Fatalf("NewProviderManager() error = %v", err)
	}
	cfg := config.Config{
		Mode: config.ModeTransform,
		ProviderDefs: map[string]config.ProviderDef{
			"main": {
				BaseURL:  "http://upstream.test",
				APIKey:   "k1,k2",
				Protocol: config.ProtocolOpenAIChat,
				Models:   map[string]config.ModelMeta{"m": {}},
			},
		},
	}
	srv := newKeyRotationServer(t, pm, cfg)

	req := &chat.ChatRequest{Model: "m", Messages: []chat.ChatMessage{{Role: "user", Content: "hi"}}}

	// Per-index client: the request must go through the proxy.
	idxClient := srv.chatClientIndex("main", 0)
	if idxClient == nil {
		t.Fatal("chatClientIndex(main, 0) = nil")
	}
	if _, err := idxClient.CreateChat(context.Background(), req); err != nil {
		t.Fatalf("CreateChat() (per-index) error = %v", err)
	}
	proxyMu.Lock()
	hits := proxyHits
	proxyMu.Unlock()
	if hits != 1 {
		t.Fatalf("proxy saw %d requests from the per-index client, want 1 (manager's proxy-aware client)", hits)
	}

	// Plain runtime-driven client: same requirement.
	raw := srv.activeChatClient("main")
	plain, ok := raw.(*chat.Client)
	if !ok {
		t.Fatalf("activeChatClient(main) = %T, want *chat.Client", raw)
	}
	if _, err := plain.CreateChat(context.Background(), req); err != nil {
		t.Fatalf("CreateChat() (plain) error = %v", err)
	}
	proxyMu.Lock()
	hits = proxyHits
	proxyMu.Unlock()
	if hits != 2 {
		t.Fatalf("proxy saw %d requests total, want 2 (both clients proxy-aware)", hits)
	}
}

// TestActiveGoogleClientUsesBootManagerKeyWhenSnapshotMgrNil checks that
// when the runtime snapshot has NO provider manager (nil ProviderMgr) but
// the Server was booted with one (Config.ProviderMgr), activeGoogleClient
// sources the API key from the boot manager: the ACTIVE single key, never
// the raw comma-separated def.APIKey list (which would leak every key into
// the ?key= query param).
func TestActiveGoogleClientUsesBootManagerKeyWhenSnapshotMgrNil(t *testing.T) {
	upstream, rec := newGoogleKeyUpstream()
	defer upstream.Close()

	cfg := config.Config{
		Mode: config.ModeTransform,
		ProviderDefs: map[string]config.ProviderDef{
			"goog": {
				BaseURL:  upstream.URL,
				APIKey:   "k1,k2",
				Protocol: config.ProtocolGoogleGenAI,
				Models:   map[string]config.ModelMeta{"m": {}},
			},
		},
	}
	// Boot manager parses the same multi-key list; the runtime snapshot
	// deliberately has NO manager (nil ProviderMgr) so activeGoogleClient
	// must fall back to the boot s.providerMgr for the key.
	pm, err := provider.NewProviderManager(
		map[string]provider.ProviderConfig{
			"goog": {
				BaseURL:    upstream.URL,
				APIKey:     "k1,k2",
				Protocol:   config.ProtocolGoogleGenAI,
				ModelNames: []string{"m"},
			},
		},
		nil,
	)
	if err != nil {
		t.Fatalf("NewProviderManager() error = %v", err)
	}
	srv := New(Config{ProviderMgr: pm, Runtime: runtime.NewRuntime(cfg, nil, nil)})

	raw := srv.activeGoogleClient("goog")
	g, ok := raw.(*google.Client)
	if !ok {
		t.Fatalf("activeGoogleClient(goog) = %T, want *google.Client", raw)
	}
	if _, err := g.GenerateContent(context.Background(), "m", &google.GenerateContentRequest{}); err != nil {
		t.Fatalf("GenerateContent() error = %v", err)
	}
	keys := rec.snapshot()
	if len(keys) != 1 {
		t.Fatalf("upstream saw %d requests, want 1", len(keys))
	}
	if keys[0] != "k1" {
		t.Fatalf("google client sent ?key=%q, want %q (boot manager's ACTIVE key, never the raw list)", keys[0], "k1")
	}

	// The boot-manager client must also be cacheable: the live-manager
	// guard (activeProviderManager) falls back to s.providerMgr too.
	srv.googleCacheMu.RLock()
	cached := srv.googleCache["goog"]
	srv.googleCacheMu.RUnlock()
	if cached == nil {
		t.Fatal("googleCache[goog] = nil, want the boot-manager client cached")
	}
}

// keyRecorder records upstream API keys in arrival order (thread-safe).
type keyRecorder struct {
	mu   sync.Mutex
	keys []string
}

func (k *keyRecorder) record(key string) {
	k.mu.Lock()
	defer k.mu.Unlock()
	k.keys = append(k.keys, key)
}

func (k *keyRecorder) count() int {
	k.mu.Lock()
	defer k.mu.Unlock()
	return len(k.keys)
}

func (k *keyRecorder) snapshot() []string {
	k.mu.Lock()
	defer k.mu.Unlock()
	out := make([]string, len(k.keys))
	copy(out, k.keys)
	return out
}

// newRotationRegistry builds a dispatch registry with the inbound client
// adapter for the given inbound protocol registered alongside the provider
// adapter (newDispatchRegistry only registers inbound OpenAI-responses).
func newRotationRegistry(t *testing.T, protocol string) *format.Registry {
	t.Helper()
	reg := newDispatchRegistry(t, protocol)
	hooks := format.CorePluginHooks{}.WithDefaults()
	switch protocol {
	case config.ProtocolAnthropic:
		adapter := anthropic.NewAnthropicClientAdapter(hooks)
		if err := reg.RegisterClient(adapter); err != nil {
			t.Fatalf("RegisterClient(anthropic inbound): %v", err)
		}
		if err := reg.RegisterClientStream(adapter); err != nil {
			t.Fatalf("RegisterClientStream(anthropic inbound): %v", err)
		}
	case config.ProtocolOpenAIChat:
		adapter := chat.NewChatClientAdapter(hooks)
		if err := reg.RegisterClient(adapter); err != nil {
			t.Fatalf("RegisterClient(chat inbound): %v", err)
		}
		if err := reg.RegisterClientStream(adapter); err != nil {
			t.Fatalf("RegisterClientStream(chat inbound): %v", err)
		}
	}
	return reg
}

// TestCoreUpstreamAnthropicRotation drives a non-streaming POST /v1/messages
// through the public handler with a multi-key anthropic provider: the first
// key returns 429, the second returns a valid MessageResponse. Rotation must
// happen inside the manager's rotating upstream client.
func TestCoreUpstreamAnthropicRotation(t *testing.T) {
	rec := &keyRecorder{}
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rec.record(r.Header.Get("x-api-key"))
		if r.Header.Get("x-api-key") == "k1" {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusTooManyRequests)
			fmt.Fprint(w, `{"type":"error","error":{"type":"rate_limit_error","message":"rate limited"}}`)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		fmt.Fprint(w, `{"id":"msg_rot","type":"message","role":"assistant","content":[{"type":"text","text":"anthropic rotated"}],"model":"mock","stop_reason":"end_turn","usage":{"input_tokens":1,"output_tokens":1}}`)
	}))
	defer upstream.Close()

	pm, err := provider.NewProviderManager(
		map[string]provider.ProviderConfig{
			"p": {
				BaseURL:  upstream.URL,
				APIKey:   "k1,k2",
				Protocol: config.ProtocolAnthropic,
				Models:   map[string]provider.ModelMeta{"upstream-model": {}},
			},
		},
		map[string]provider.ModelRoute{
			"alias": {Provider: "p", Name: "upstream-model"},
		},
	)
	if err != nil {
		t.Fatalf("NewProviderManager() error = %v", err)
	}
	srv := New(Config{
		ProviderMgr:     pm,
		AdapterRegistry: newRotationRegistry(t, config.ProtocolAnthropic),
	})

	body := bytes.NewBufferString(`{"model":"alias","max_tokens":64,"messages":[{"role":"user","content":"hello"}]}`)
	recorder := httptest.NewRecorder()
	srv.ServeHTTP(recorder, httptest.NewRequest(http.MethodPost, "/v1/messages", body))

	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", recorder.Code, recorder.Body.String())
	}
	if !bytes.Contains(recorder.Body.Bytes(), []byte("anthropic rotated")) {
		t.Fatalf("response body missing rotated content: %s", recorder.Body.String())
	}
	if keys := rec.snapshot(); !slices.Equal(keys, []string{"k1", "k2"}) {
		t.Fatalf("upstream keys = %v, want [k1 k2]", keys)
	}
	if idx := pm.ActiveKeyIndex("p"); idx != 1 {
		t.Fatalf("ActiveKeyIndex(p) = %d, want 1", idx)
	}
}

// TestCoreUpstreamChatRotation drives a non-streaming POST
// /v1/chat/completions through the public handler with a multi-key
// openai-chat provider: Bearer k1 returns 429, Bearer k2 returns a valid
// chat completion.
func TestCoreUpstreamChatRotation(t *testing.T) {
	rec := &keyRecorder{}
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rec.record(r.Header.Get("Authorization"))
		if r.Header.Get("Authorization") == "Bearer k1" {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusTooManyRequests)
			fmt.Fprint(w, `{"error":{"message":"rate limited","type":"rate_limit_error"}}`)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		fmt.Fprint(w, `{"id":"chatcmpl_rot","object":"chat.completion","created":1,"model":"mock","choices":[{"index":0,"message":{"role":"assistant","content":"chat rotated"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`)
	}))
	defer upstream.Close()

	pm, err := provider.NewProviderManager(
		map[string]provider.ProviderConfig{
			"p": {
				BaseURL:  upstream.URL,
				APIKey:   "k1,k2",
				Protocol: config.ProtocolOpenAIChat,
				Models:   map[string]provider.ModelMeta{"upstream-model": {}},
			},
		},
		map[string]provider.ModelRoute{
			"alias": {Provider: "p", Name: "upstream-model"},
		},
	)
	if err != nil {
		t.Fatalf("NewProviderManager() error = %v", err)
	}
	// The chat rotating caller builds per-index clients from the runtime
	// snapshot's provider defs.
	cfg := config.Config{
		Mode: config.ModeTransform,
		ProviderDefs: map[string]config.ProviderDef{
			"p": {
				BaseURL:  upstream.URL,
				APIKey:   "k1,k2",
				Protocol: config.ProtocolOpenAIChat,
				Models:   map[string]config.ModelMeta{"upstream-model": {}},
			},
		},
	}
	srv := New(Config{
		ProviderMgr:     pm,
		AdapterRegistry: newRotationRegistry(t, config.ProtocolOpenAIChat),
		Runtime:         runtime.NewRuntime(cfg, pm, nil),
	})

	body := bytes.NewBufferString(`{"model":"alias","messages":[{"role":"user","content":"hello"}]}`)
	recorder := httptest.NewRecorder()
	srv.ServeHTTP(recorder, httptest.NewRequest(http.MethodPost, "/v1/chat/completions", body))

	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", recorder.Code, recorder.Body.String())
	}
	if !bytes.Contains(recorder.Body.Bytes(), []byte("chat rotated")) {
		t.Fatalf("response body missing rotated content: %s", recorder.Body.String())
	}
	if keys := rec.snapshot(); !slices.Equal(keys, []string{"Bearer k1", "Bearer k2"}) {
		t.Fatalf("upstream keys = %v, want [Bearer k1 Bearer k2]", keys)
	}
	if idx := pm.ActiveKeyIndex("p"); idx != 1 {
		t.Fatalf("ActiveKeyIndex(p) = %d, want 1", idx)
	}
}

// TestPassthroughRotation drives a non-streaming POST /v1/responses through
// the passthrough handler with a multi-key openai-response provider: Bearer
// k1 returns 429 with body "quota", Bearer k2 returns a valid response.
func TestPassthroughRotation(t *testing.T) {
	rec := &keyRecorder{}
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rec.record(r.Header.Get("Authorization"))
		if r.Header.Get("Authorization") == "Bearer k1" {
			w.WriteHeader(http.StatusTooManyRequests)
			fmt.Fprint(w, "quota")
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		fmt.Fprint(w, `{"id":"resp_rot","object":"response","status":"completed","output":[]}`)
	}))
	defer upstream.Close()

	pm, err := provider.NewProviderManager(
		map[string]provider.ProviderConfig{
			"openai": {
				BaseURL:  upstream.URL,
				APIKey:   "k1,k2",
				Protocol: config.ProtocolOpenAIResponse,
			},
		},
		map[string]provider.ModelRoute{
			"rot": {Provider: "openai", Name: "gpt-upstream"},
		},
	)
	if err != nil {
		t.Fatalf("NewProviderManager() error = %v", err)
	}
	srv := New(Config{ProviderMgr: pm})

	body := bytes.NewBufferString(`{"model":"rot","input":"hello"}`)
	recorder := httptest.NewRecorder()
	srv.ServeHTTP(recorder, httptest.NewRequest(http.MethodPost, "/v1/responses", body))

	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", recorder.Code, recorder.Body.String())
	}
	if keys := rec.snapshot(); !slices.Equal(keys, []string{"Bearer k1", "Bearer k2"}) {
		t.Fatalf("upstream keys = %v, want [Bearer k1 Bearer k2]", keys)
	}
}

// TestPassthroughRotationFullFailureReplaysActiveKeyBody is the full-failure
// variant: both keys return 429 (k1 body "first", k2 body "second") and the
// response must be the ACTIVE key's 429 replayed verbatim.
func TestPassthroughRotationFullFailureReplaysActiveKeyBody(t *testing.T) {
	rec := &keyRecorder{}
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		key := r.Header.Get("Authorization")
		rec.record(key)
		w.WriteHeader(http.StatusTooManyRequests)
		if key == "Bearer k1" {
			fmt.Fprint(w, "first")
			return
		}
		fmt.Fprint(w, "second")
	}))
	defer upstream.Close()

	pm, err := provider.NewProviderManager(
		map[string]provider.ProviderConfig{
			"openai": {
				BaseURL:  upstream.URL,
				APIKey:   "k1,k2",
				Protocol: config.ProtocolOpenAIResponse,
			},
		},
		map[string]provider.ModelRoute{
			"rot": {Provider: "openai", Name: "gpt-upstream"},
		},
	)
	if err != nil {
		t.Fatalf("NewProviderManager() error = %v", err)
	}
	srv := New(Config{ProviderMgr: pm})

	body := bytes.NewBufferString(`{"model":"rot","input":"hello"}`)
	recorder := httptest.NewRecorder()
	srv.ServeHTTP(recorder, httptest.NewRequest(http.MethodPost, "/v1/responses", body))

	if recorder.Code != http.StatusTooManyRequests {
		t.Fatalf("status = %d, body = %s", recorder.Code, recorder.Body.String())
	}
	if got := recorder.Body.String(); got != "first" {
		t.Fatalf("response body = %q, want the active-key body %q verbatim", got, "first")
	}
	if keys := rec.snapshot(); !slices.Equal(keys, []string{"Bearer k1", "Bearer k2"}) {
		t.Fatalf("upstream keys = %v, want [Bearer k1 Bearer k2]", keys)
	}
}

// TestPassthroughRotationNoAdvanceOnNon2xx is the F1 variant: k1 returns 429
// (rotatable) and k2 returns 500 (non-rotatable). The 500 must be proxied to
// the caller, but the active index must NOT advance to k2 — advance only on
// a 2xx success.
func TestPassthroughRotationNoAdvanceOnNon2xx(t *testing.T) {
	rec := &keyRecorder{}
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rec.record(r.Header.Get("Authorization"))
		if r.Header.Get("Authorization") == "Bearer k1" {
			w.WriteHeader(http.StatusTooManyRequests)
			fmt.Fprint(w, "quota")
			return
		}
		w.WriteHeader(http.StatusInternalServerError)
		fmt.Fprint(w, "boom")
	}))
	defer upstream.Close()

	pm, err := provider.NewProviderManager(
		map[string]provider.ProviderConfig{
			"openai": {
				BaseURL:  upstream.URL,
				APIKey:   "k1,k2",
				Protocol: config.ProtocolOpenAIResponse,
			},
		},
		map[string]provider.ModelRoute{
			"rot": {Provider: "openai", Name: "gpt-upstream"},
		},
	)
	if err != nil {
		t.Fatalf("NewProviderManager() error = %v", err)
	}
	srv := New(Config{ProviderMgr: pm})

	body := bytes.NewBufferString(`{"model":"rot","input":"hello"}`)
	recorder := httptest.NewRecorder()
	srv.ServeHTTP(recorder, httptest.NewRequest(http.MethodPost, "/v1/responses", body))

	if recorder.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, body = %s", recorder.Code, recorder.Body.String())
	}
	if got := recorder.Body.String(); got != "boom" {
		t.Fatalf("response body = %q, want the k2 500 body %q", got, "boom")
	}
	if keys := rec.snapshot(); !slices.Equal(keys, []string{"Bearer k1", "Bearer k2"}) {
		t.Fatalf("upstream keys = %v, want [Bearer k1 Bearer k2]", keys)
	}
	if idx := pm.ActiveKeyIndex("openai"); idx != 0 {
		t.Fatalf("ActiveKeyIndex(openai) = %d, want 0 (no advance on non-2xx)", idx)
	}
}

// TestPassthroughRotationStartsAtActiveIndex pins the F8 uniform idx->key
// fetch: the k==0 attempt must use the key at the LIVE active index, not a
// captured one. Start the provider at index 1 (k2 active): k2 returns 429,
// k1 returns 200 — the first upstream call must carry Bearer k2.
func TestPassthroughRotationStartsAtActiveIndex(t *testing.T) {
	rec := &keyRecorder{}
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rec.record(r.Header.Get("Authorization"))
		if r.Header.Get("Authorization") == "Bearer k2" {
			w.WriteHeader(http.StatusTooManyRequests)
			fmt.Fprint(w, "quota")
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		fmt.Fprint(w, `{"id":"resp_rot","object":"response","status":"completed","output":[]}`)
	}))
	defer upstream.Close()

	pm, err := provider.NewProviderManager(
		map[string]provider.ProviderConfig{
			"openai": {
				BaseURL:  upstream.URL,
				APIKey:   "k1,k2",
				Protocol: config.ProtocolOpenAIResponse,
			},
		},
		map[string]provider.ModelRoute{
			"rot": {Provider: "openai", Name: "gpt-upstream"},
		},
	)
	if err != nil {
		t.Fatalf("NewProviderManager() error = %v", err)
	}
	if !pm.AdvanceKeyIndex("openai", 0, 1) {
		t.Fatal("AdvanceKeyIndex(openai,0,1) = false, want true")
	}
	srv := New(Config{ProviderMgr: pm})

	body := bytes.NewBufferString(`{"model":"rot","input":"hello"}`)
	recorder := httptest.NewRecorder()
	srv.ServeHTTP(recorder, httptest.NewRequest(http.MethodPost, "/v1/responses", body))

	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", recorder.Code, recorder.Body.String())
	}
	if keys := rec.snapshot(); !slices.Equal(keys, []string{"Bearer k2", "Bearer k1"}) {
		t.Fatalf("upstream keys = %v, want [Bearer k2 Bearer k1]", keys)
	}
	if idx := pm.ActiveKeyIndex("openai"); idx != 0 {
		t.Fatalf("ActiveKeyIndex(openai) = %d, want 0 after 2xx at index 0", idx)
	}
}

// TestChatClientCacheRefusesClientFromRetiredManager reproduces the R2
// race: request A passes invalidateClientCacheOnManagerChange (records
// pm1), builds a client with pm1's key, then a runtime reload swaps in
// pm2 and request B invalidates (records pm2) and caches a pm2 client
// BEFORE A re-acquires the write lock. A's store must be refused: the
// caches belong to pm2 now, and A's client carries pm1's (retired) key.
// After the race, the served client's Authorization header must be the
// NEW key.
func TestChatClientCacheRefusesClientFromRetiredManager(t *testing.T) {
	var mu sync.Mutex
	var auth string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		auth = r.Header.Get("Authorization")
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"id":"chatcmpl-1","object":"chat.completion","created":1,"model":"m","choices":[{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}]}`)
	}))
	defer upstream.Close()

	newCfg := func(apiKey string) config.Config {
		return config.Config{
			Mode: config.ModeTransform,
			ProviderDefs: map[string]config.ProviderDef{
				"main": {
					BaseURL:  upstream.URL,
					APIKey:   apiKey,
					Protocol: config.ProtocolOpenAIChat,
					Models:   map[string]config.ModelMeta{"m": {}},
				},
			},
		}
	}

	cfg := newCfg("k1")
	pm1, err := provider.NewProviderManager(
		map[string]provider.ProviderConfig{
			"main": {
				BaseURL:    upstream.URL,
				APIKey:     "k1",
				Protocol:   config.ProtocolOpenAIChat,
				ModelNames: []string{"m"},
			},
		},
		nil,
	)
	if err != nil {
		t.Fatalf("NewProviderManager() error = %v", err)
	}
	rt := runtime.NewRuntime(cfg, pm1, nil)
	srv := New(Config{ProviderMgr: pm1, Runtime: rt})

	// Request A's early steps: invalidates on pm1 and caches nothing yet.
	if first := srv.activeChatClient("main"); first == nil {
		t.Fatal("activeChatClient(main) = nil before reload")
	}
	// The reload swaps in pm2; request B invalidates (records pm2) and
	// caches a pm2-keyed client.
	if err := rt.Reload(newCfg("k2")); err != nil {
		t.Fatalf("Reload() error = %v", err)
	}
	if second := srv.activeChatClient("main"); second == nil {
		t.Fatal("activeChatClient(main) = nil after reload")
	}

	// Request A's late store: A had captured pm1 (now retired) and built
	// a pm1-keyed client. The store must be refused.
	stale := chat.NewClient(chat.ClientConfig{BaseURL: upstream.URL, APIKey: "k1"})
	if srv.cacheChatClient(pm1, "main", stale) {
		t.Fatal("cacheChatClient(pm1, main, stale) = true, want false: the caches belong to the post-reload manager")
	}
	// Same guard on the per-index cache path.
	staleIdx := chat.NewClient(chat.ClientConfig{BaseURL: upstream.URL, APIKey: "k1"})
	if srv.cacheChatClient(pm1, "main\x00"+"0", staleIdx) {
		t.Fatal("cacheChatClient(pm1, per-index key, stale) = true, want false")
	}
	// Same guard on the google cache path.
	staleGoogle := google.NewClient(google.ClientConfig{BaseURL: upstream.URL, APIKey: "k1"})
	if srv.cacheGoogleClient(pm1, "main", staleGoogle) {
		t.Fatal("cacheGoogleClient(pm1, main, stale) = true, want false")
	}

	// The cache must still hold the pm2 client and serve the NEW key.
	srv.clientCacheMu.RLock()
	cached := srv.clientCache["main"]
	srv.clientCacheMu.RUnlock()
	if cached == nil {
		t.Fatal("clientCache[main] = nil after the refused store")
	}
	if cached == stale {
		t.Fatal("clientCache[main] retained a client built from a retired manager's key")
	}
	req := &chat.ChatRequest{Model: "m", Messages: []chat.ChatMessage{{Role: "user", Content: "hi"}}}
	if _, err := cached.CreateChat(context.Background(), req); err != nil {
		t.Fatalf("CreateChat() error = %v", err)
	}
	mu.Lock()
	got := auth
	mu.Unlock()
	if got != "Bearer k2" {
		t.Fatalf("served client Authorization = %q, want %q (NEW key)", got, "Bearer k2")
	}

	// The google cache must be empty of stale clients too: the refused
	// store must not have planted the pm1-keyed google client.
	srv.googleCacheMu.RLock()
	cachedGoogle := srv.googleCache["main"]
	srv.googleCacheMu.RUnlock()
	if cachedGoogle == staleGoogle {
		t.Fatal("googleCache[main] retained a client built from a retired manager's key")
	}
}

// TestChatClientStoreAfterMidBuildReloadNotCached reproduces the R5
// window: a request captures the manager (pm1) and builds a client, a
// runtime reload swaps in a NEW manager, and NO cache read runs in
// between — so the recorded clientCacheMgr still equals pm1 when the
// build finishes and the R2-era store guard (clientCacheMgr == mgr)
// accepts the store. A reader whose invalidation check already passed
// pre-reload could then find the retired-manager client in its cache
// lookup. The store must compare against the LIVE manager and refuse.
func TestChatClientStoreAfterMidBuildReloadNotCached(t *testing.T) {
	var mu sync.Mutex
	var auth string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		auth = r.Header.Get("Authorization")
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"id":"chatcmpl-1","object":"chat.completion","created":1,"model":"m","choices":[{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}]}`)
	}))
	defer upstream.Close()

	newCfg := func(apiKey string) config.Config {
		return config.Config{
			Mode: config.ModeTransform,
			ProviderDefs: map[string]config.ProviderDef{
				"main": {
					BaseURL:  upstream.URL,
					APIKey:   apiKey,
					Protocol: config.ProtocolOpenAIChat,
					Models:   map[string]config.ModelMeta{"m": {}},
				},
			},
		}
	}

	cfg := newCfg("k1")
	pm1, err := provider.NewProviderManager(
		map[string]provider.ProviderConfig{
			"main": {
				BaseURL:    upstream.URL,
				APIKey:     "k1",
				Protocol:   config.ProtocolOpenAIChat,
				ModelNames: []string{"m"},
			},
		},
		nil,
	)
	if err != nil {
		t.Fatalf("NewProviderManager() error = %v", err)
	}
	rt := runtime.NewRuntime(cfg, pm1, nil)
	srv := New(Config{ProviderMgr: pm1, Runtime: rt})

	// The in-flight request has already run its invalidation check on
	// pm1 (recording pm1 as the cache owner) and captured pm1.
	srv.invalidateClientCacheOnManagerChange()
	inflight := chat.NewClient(chat.ClientConfig{BaseURL: upstream.URL, APIKey: "k1"})
	staleGoogle := google.NewClient(google.ClientConfig{BaseURL: upstream.URL, APIKey: "k1"})

	// The reload lands between the capture and the store, with no
	// intervening cache read: clientCacheMgr still records pm1.
	if err := rt.Reload(newCfg("k2")); err != nil {
		t.Fatalf("Reload() error = %v", err)
	}

	// The stores must be refused: the LIVE manager is the post-reload
	// one, even though clientCacheMgr still equals pm1.
	if srv.cacheChatClient(pm1, "main", inflight) {
		t.Fatal("cacheChatClient(pm1, main, inflight) = true, want false: a reload retired pm1 between the capture and this store")
	}
	if srv.cacheGoogleClient(pm1, "main", staleGoogle) {
		t.Fatal("cacheGoogleClient(pm1, main, staleGoogle) = true, want false: a reload retired pm1 between the capture and this store")
	}

	// Nothing from the retired manager may sit in the caches.
	srv.clientCacheMu.RLock()
	cached := srv.clientCache["main"]
	srv.clientCacheMu.RUnlock()
	if cached != nil {
		t.Fatal("clientCache[main] holds a client after the refused store, want no entry")
	}
	srv.googleCacheMu.RLock()
	cachedGoogle := srv.googleCache["main"]
	srv.googleCacheMu.RUnlock()
	if cachedGoogle != nil {
		t.Fatal("googleCache[main] holds a client after the refused store, want no entry")
	}

	// The next read serves the NEW manager's key.
	raw := srv.activeChatClient("main")
	client, ok := raw.(*chat.Client)
	if !ok {
		t.Fatalf("activeChatClient(main) after reload = %T, want *chat.Client", raw)
	}
	req := &chat.ChatRequest{Model: "m", Messages: []chat.ChatMessage{{Role: "user", Content: "hi"}}}
	if _, err := client.CreateChat(context.Background(), req); err != nil {
		t.Fatalf("CreateChat() error = %v", err)
	}
	mu.Lock()
	got := auth
	mu.Unlock()
	if got != "Bearer k2" {
		t.Fatalf("upstream Authorization = %q, want %q (post-reload key)", got, "Bearer k2")
	}
}

// TestActiveChatClientRacingReloadNeverReturnsNil reproduces the R4
// regression: a client build overlapping a runtime reload was dropped
// (nil returned) whenever another reader had already invalidated with
// the new manager, surfacing to consumers as spurious
// "no chat client for provider" 502s under live management-API reload
// traffic. Every successful build must yield a working client: cached
// when the manager still owns the caches, or served uncached when it
// swapped mid-build. Run with -race.
func TestActiveChatClientRacingReloadNeverReturnsNil(t *testing.T) {
	rec := &keyRecorder{}
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rec.record(r.Header.Get("Authorization"))
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"id":"chatcmpl-1","object":"chat.completion","created":1,"model":"m","choices":[{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}]}`)
	}))
	defer upstream.Close()

	newCfg := func(apiKey string) config.Config {
		return config.Config{
			Mode: config.ModeTransform,
			ProviderDefs: map[string]config.ProviderDef{
				"main": {
					BaseURL:  upstream.URL,
					APIKey:   apiKey,
					Protocol: config.ProtocolOpenAIChat,
					Models:   map[string]config.ModelMeta{"m": {}},
				},
			},
		}
	}

	cfg1, cfg2 := newCfg("k1"), newCfg("k2")
	pm, err := provider.NewProviderManager(
		map[string]provider.ProviderConfig{
			"main": {
				BaseURL:    upstream.URL,
				APIKey:     "k1",
				Protocol:   config.ProtocolOpenAIChat,
				ModelNames: []string{"m"},
			},
		},
		nil,
	)
	if err != nil {
		t.Fatalf("NewProviderManager() error = %v", err)
	}
	rt := runtime.NewRuntime(cfg1, pm, nil)
	srv := New(Config{ProviderMgr: pm, Runtime: rt})

	const workers = 4
	const iters = 150
	var mu sync.Mutex
	nils := 0
	served := 0

	var reloadWG, readersWG sync.WaitGroup
	stop := make(chan struct{})
	reloadWG.Add(1)
	go func() {
		defer reloadWG.Done()
		for i := 0; ; i++ {
			select {
			case <-stop:
				return
			default:
			}
			cfg := cfg1
			if i%2 == 1 {
				cfg = cfg2
			}
			if err := rt.Reload(cfg); err != nil {
				t.Errorf("Reload() error = %v", err)
				return
			}
		}
	}()
	for w := 0; w < workers; w++ {
		readersWG.Add(1)
		go func() {
			defer readersWG.Done()
			for i := 0; i < iters; i++ {
				use := func(c *chat.Client) {
					if c == nil {
						mu.Lock()
						nils++
						mu.Unlock()
						return
					}
					// Fresh request per call: CreateChat mutates it
					// (req.Stream = false), so it must not be shared
					// across goroutines under -race.
					req := &chat.ChatRequest{Model: "m", Messages: []chat.ChatMessage{{Role: "user", Content: "hi"}}}
					if _, err := c.CreateChat(context.Background(), req); err != nil {
						t.Errorf("CreateChat() error = %v", err)
						return
					}
					mu.Lock()
					served++
					mu.Unlock()
				}
				raw := srv.activeChatClient("main")
				client, ok := raw.(*chat.Client)
				if !ok {
					t.Errorf("activeChatClient(main) = %T (%v), want *chat.Client", raw, raw)
					continue
				}
				use(client)
				use(srv.chatClientIndex("main", 0))
			}
		}()
	}
	// Wait for the readers, then stop the reload storm.
	readersWG.Wait()
	close(stop)
	reloadWG.Wait()

	if nils != 0 {
		t.Fatalf("%d nil client returns while racing reloads, want 0", nils)
	}
	want := 2 * workers * iters
	if served != want {
		t.Fatalf("served %d requests, want %d (every built client must be usable)", served, want)
	}
	for _, k := range rec.snapshot() {
		if k != "Bearer k1" && k != "Bearer k2" {
			t.Fatalf("served Authorization = %q, want a valid key (Bearer k1 or Bearer k2)", k)
		}
	}
}

// TestServedClientsPairKeyWithEndpointOfSameGeneration reproduces the R6
// defect: the client build sites read the provider def and the API key
// from DIFFERENT runtime snapshot reads, so a reload landing between the
// reads builds a MISMATCHED client (old BaseURL + new key, or new BaseURL
// + old key) — a credential-disclosure window where the NEW key is sent
// to a retired endpoint (or vice versa). The observable invariant: every
// SERVED request must carry a (endpoint, key) pair from ONE provider-def
// generation. Each generation gets its own upstream server, so a
// mismatched pair is directly observable at the receiving upstream.
// Run with -race.
func TestServedClientsPairKeyWithEndpointOfSameGeneration(t *testing.T) {
	// chatUpN is the OpenAI-chat upstream of generation N; googUpN is
	// the google-genai upstream of generation N. Each records every
	// credential it receives.
	chatUp1, chatRec1 := newChatAuthUpstream()
	defer chatUp1.Close()
	chatUp2, chatRec2 := newChatAuthUpstream()
	defer chatUp2.Close()
	googUp1, googRec1 := newGoogleKeyUpstream()
	defer googUp1.Close()
	googUp2, googRec2 := newGoogleKeyUpstream()
	defer googUp2.Close()

	// genCfg builds the config of generation N: chat def points at
	// chatUpN with key kN, google def at googUpN with key kN.
	genCfg := func(n int, chatURL, googURL string) config.Config {
		key := fmt.Sprintf("k%d", n)
		return config.Config{
			Mode: config.ModeTransform,
			ProviderDefs: map[string]config.ProviderDef{
				"chat": {
					BaseURL:  chatURL,
					APIKey:   key,
					Protocol: config.ProtocolOpenAIChat,
					Models:   map[string]config.ModelMeta{"m": {}},
				},
				"goog": {
					BaseURL:    googURL,
					APIKey:     key,
					Protocol:   config.ProtocolGoogleGenAI,
					APIVersion: "v1",
					Models:     map[string]config.ModelMeta{"m": {}},
				},
			},
		}
	}
	cfg1 := genCfg(1, chatUp1.URL, googUp1.URL)
	cfg2 := genCfg(2, chatUp2.URL, googUp2.URL)

	pm, err := provider.NewProviderManager(
		map[string]provider.ProviderConfig{
			"chat": {
				BaseURL:    chatUp1.URL,
				APIKey:     "k1",
				Protocol:   config.ProtocolOpenAIChat,
				ModelNames: []string{"m"},
			},
			"goog": {
				BaseURL:    googUp1.URL,
				APIKey:     "k1",
				Protocol:   config.ProtocolGoogleGenAI,
				ModelNames: []string{"m"},
			},
		},
		nil,
	)
	if err != nil {
		t.Fatalf("NewProviderManager() error = %v", err)
	}
	rt := runtime.NewRuntime(cfg1, pm, nil)
	srv := New(Config{ProviderMgr: pm, Runtime: rt})

	const workers = 8
	const iters = 200
	var reloadWG, readersWG sync.WaitGroup
	stop := make(chan struct{})
	reloadWG.Add(1)
	go func() {
		defer reloadWG.Done()
		for i := 0; ; i++ {
			select {
			case <-stop:
				return
			default:
			}
			cfg := cfg1
			if i%2 == 1 {
				cfg = cfg2
			}
			if err := rt.Reload(cfg); err != nil {
				t.Errorf("Reload() error = %v", err)
				return
			}
		}
	}()
	for w := 0; w < workers; w++ {
		readersWG.Add(1)
		go func() {
			defer readersWG.Done()
			for i := 0; i < iters; i++ {
				req := &chat.ChatRequest{Model: "m", Messages: []chat.ChatMessage{{Role: "user", Content: "hi"}}}
				if raw := srv.activeChatClient("chat"); raw != nil {
					if c, ok := raw.(*chat.Client); ok {
						if _, err := c.CreateChat(context.Background(), req); err != nil {
							t.Errorf("chat CreateChat() error = %v", err)
						}
					}
				}
				if c := srv.chatClientIndex("chat", 0); c != nil {
					if _, err := c.CreateChat(context.Background(), req); err != nil {
						t.Errorf("chatIndex CreateChat() error = %v", err)
					}
				}
				if raw := srv.activeGoogleClient("goog"); raw != nil {
					if g, ok := raw.(*google.Client); ok {
						if _, err := g.GenerateContent(context.Background(), "m", &google.GenerateContentRequest{}); err != nil {
							t.Errorf("google GenerateContent() error = %v", err)
						}
					}
				}
			}
		}()
	}
	readersWG.Wait()
	close(stop)
	reloadWG.Wait()

	// THE INVARIANT: each generation's upstream only ever receives its
	// own key. A cross-generation (endpoint, key) pair is the R6
	// mismatch — new key disclosed to a retired endpoint, or old key
	// sent to a new endpoint.
	for _, tc := range []struct {
		name string
		rec  *keyRecorder
		want string
	}{
		{"chat upstream gen1 (k1 endpoint)", chatRec1, "Bearer k1"},
		{"chat upstream gen2 (k2 endpoint)", chatRec2, "Bearer k2"},
		{"google upstream gen1 (k1 endpoint)", googRec1, "k1"},
		{"google upstream gen2 (k2 endpoint)", googRec2, "k2"},
	} {
		keys := tc.rec.snapshot()
		if len(keys) == 0 {
			t.Fatalf("%s: no requests recorded, want the storm to have exercised it", tc.name)
		}
		for _, k := range keys {
			if k != tc.want {
				t.Fatalf("%s: received credential %q, want %q (a mismatched cross-generation key/endpoint pair was served)", tc.name, k, tc.want)
			}
		}
	}
}

// newChatAuthUpstream starts an OpenAI-chat upstream recording every
// request's Authorization header.
func newChatAuthUpstream() (*httptest.Server, *keyRecorder) {
	rec := &keyRecorder{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rec.record(r.Header.Get("Authorization"))
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"id":"chatcmpl-1","object":"chat.completion","created":1,"model":"m","choices":[{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}]}`)
	}))
	return srv, rec
}

// newGoogleKeyUpstream starts a google-genai (Gemini API key in the query
// string) upstream recording every request's key param.
func newGoogleKeyUpstream() (*httptest.Server, *keyRecorder) {
	rec := &keyRecorder{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rec.record(r.URL.Query().Get("key"))
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"candidates":[]}`)
	}))
	return srv, rec
}
