// Package runtime tests for key rotation store re-attachment across
// manager rebuilds (Reload / ValidateCandidate) and bootstrapping.
package runtime_test

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"providerbridge/internal/config"
	"providerbridge/internal/protocol/chat"
	"providerbridge/internal/service/provider"
	"providerbridge/internal/service/runtime"
)

// fakeKeyRotationStore is an in-memory provider.KeyRotationStore recording
// persist calls (thread-safe; persist is asynchronous).
type fakeKeyRotationStore struct {
	mu    sync.Mutex
	idx   map[string]int
	calls []string
}

func (f *fakeKeyRotationStore) LoadProviderKeyIndexes(_ context.Context) (map[string]int, error) {
	return f.idx, nil
}

func (f *fakeKeyRotationStore) SetProviderKeyIndex(_ context.Context, providerKey string, idx int) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, fmt.Sprintf("%s=%d", providerKey, idx))
	return nil
}

func (f *fakeKeyRotationStore) has(providerKey string, idx int) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	want := fmt.Sprintf("%s=%d", providerKey, idx)
	for _, c := range f.calls {
		if c == want {
			return true
		}
	}
	return false
}

// rotationTestConfig returns a valid transform-mode config with one
// multi-key (k1,k2) openai-chat provider "p".
func rotationTestConfig(upstreamURL string) config.Config {
	return config.Config{
		Mode:         config.ModeTransform,
		Addr:         "127.0.0.1:38440",
		DefaultModel: "alias",
		Routes: map[string]config.RouteEntry{
			"alias": {Provider: "p", Model: "m"},
		},
		ProviderDefs: map[string]config.ProviderDef{
			"p": {
				BaseURL:  upstreamURL,
				APIKey:   "k1,k2",
				Protocol: config.ProtocolOpenAIChat,
				Models:   map[string]config.ModelMeta{"m": {}},
			},
		},
		Cache: config.CacheConfig{Mode: "off"},
	}
}

// TestRuntimeReloadKeepsRotationStore proves that a manager rebuilt by
// Runtime.Reload keeps the key rotation persistence store: forcing an index
// advance on the rebuilt manager persists through the store attached at
// boot.
func TestRuntimeReloadKeepsRotationStore(t *testing.T) {
	var mu sync.Mutex
	var auth string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		auth = r.Header.Get("Authorization")
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		if auth == "Bearer k1" {
			w.WriteHeader(http.StatusTooManyRequests)
			fmt.Fprint(w, `{"error":{"message":"rate limited k1","type":"rate_limit_error"}}`)
			return
		}
		fmt.Fprint(w, `{"id":"chatcmpl-1","object":"chat.completion","created":1,"model":"m","choices":[{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}]}`)
	}))
	defer upstream.Close()

	cfg := rotationTestConfig(upstream.URL)
	pm, err := provider.NewProviderManager(
		map[string]provider.ProviderConfig{
			"p": {
				BaseURL:    upstream.URL,
				APIKey:     "k1,k2",
				Protocol:   config.ProtocolOpenAIChat,
				ModelNames: []string{"m"},
			},
		},
		map[string]provider.ModelRoute{
			"alias": {Provider: "p", Name: "m"},
		},
	)
	if err != nil {
		t.Fatalf("NewProviderManager() error = %v", err)
	}

	fake := &fakeKeyRotationStore{}
	rt := runtime.NewRuntime(cfg, pm, nil)
	rt.SetKeyRotationStore(fake)

	// Rebuild the manager via Reload: the rebuilt manager must still have
	// the rotation store attached.
	if err := rt.Reload(cfg); err != nil {
		t.Fatalf("Reload() error = %v", err)
	}
	snap := rt.Current()
	if snap.ProviderMgr == nil {
		t.Fatal("Current().ProviderMgr is nil after Reload")
	}

	// Force an index advance on the rebuilt manager through the rotating
	// chat caller: k1 returns 429, k2 succeeds → active 0 -> 1, persisted.
	caller := provider.NewRotatingChatClient(snap.ProviderMgr, "p", func(idx int) *chat.Client {
		return chat.NewClient(chat.ClientConfig{
			BaseURL: upstream.URL,
			APIKey:  snap.ProviderMgr.ProviderAPIKeyIndex("p", idx),
		})
	})
	req := &chat.ChatRequest{Model: "m", Messages: []chat.ChatMessage{{Role: "user", Content: "hi"}}}
	if _, err := caller.CreateChat(context.Background(), req); err != nil {
		t.Fatalf("CreateChat() error = %v", err)
	}
	if idx := snap.ProviderMgr.ActiveKeyIndex("p"); idx != 1 {
		t.Fatalf("ActiveKeyIndex(p) = %d, want 1 after rotation", idx)
	}

	deadline := time.Now().Add(1 * time.Second)
	for !fake.has("p", 1) {
		if time.Now().After(deadline) {
			t.Fatal("rotation store saw no persist of (p,1) within 1s; the rebuilt manager lost the store")
		}
		time.Sleep(5 * time.Millisecond)
	}
}
