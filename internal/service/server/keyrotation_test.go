// Package server tests for API-key rotation: the per-index chat client
// cache and the active chat caller selection.
package server

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"providerbridge/internal/config"
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
