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
	"sync"
	"testing"

	"providerbridge/internal/config"
	"providerbridge/internal/format"
	"providerbridge/internal/protocol/anthropic"
	"providerbridge/internal/protocol/chat"
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

func equalKeys(got, want []string) bool {
	if len(got) != len(want) {
		return false
	}
	for i := range got {
		if got[i] != want[i] {
			return false
		}
	}
	return true
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
	if keys := rec.snapshot(); !equalKeys(keys, []string{"k1", "k2"}) {
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
	if keys := rec.snapshot(); !equalKeys(keys, []string{"Bearer k1", "Bearer k2"}) {
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
	if keys := rec.snapshot(); !equalKeys(keys, []string{"Bearer k1", "Bearer k2"}) {
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
	if keys := rec.snapshot(); !equalKeys(keys, []string{"Bearer k1", "Bearer k2"}) {
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
	if keys := rec.snapshot(); !equalKeys(keys, []string{"Bearer k1", "Bearer k2"}) {
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
	if keys := rec.snapshot(); !equalKeys(keys, []string{"Bearer k2", "Bearer k1"}) {
		t.Fatalf("upstream keys = %v, want [Bearer k2 Bearer k1]", keys)
	}
	if idx := pm.ActiveKeyIndex("openai"); idx != 0 {
		t.Fatalf("ActiveKeyIndex(openai) = %d, want 0 after 2xx at index 0", idx)
	}
}
