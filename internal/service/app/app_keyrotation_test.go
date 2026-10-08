// Package app tests for boot-time key rotation: the boot chatClients map
// must be built from the provider's ACTIVE API key, never the raw
// comma-separated def.APIKey (S-5-7 header-leak fix).
package app

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"providerbridge/internal/config"
	"providerbridge/internal/protocol/chat"
	"providerbridge/internal/service/provider"
)

func TestNewBootChatClientUsesActiveKey(t *testing.T) {
	// httptest upstream that records the Authorization header of every
	// request and replies with a minimal chat.completion body.
	var mu sync.Mutex
	var auths []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		auths = append(auths, r.Header.Get("Authorization"))
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"resp_1","object":"chat.completion","choices":[{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}]}`))
	}))
	defer srv.Close()

	chatReq := func() *chat.ChatRequest {
		return &chat.ChatRequest{
			Model:    "test-model",
			Messages: []chat.ChatMessage{{Role: "user", Content: "hi"}},
		}
	}

	t.Run("multi-key provider sends the ACTIVE key only", func(t *testing.T) {
		pm, err := provider.NewProviderManager(map[string]provider.ProviderConfig{
			"p": {BaseURL: srv.URL, APIKey: "k1,k2"},
		}, nil)
		if err != nil {
			t.Fatalf("NewProviderManager() error = %v", err)
		}
		def := config.ProviderDef{BaseURL: srv.URL, APIKey: "k1,k2", Protocol: config.ProtocolOpenAIChat}
		c := newBootChatClient(pm, def, "p", srv.Client())
		if _, err := c.CreateChat(context.Background(), chatReq()); err != nil {
			t.Fatalf("CreateChat() error = %v", err)
		}
		mu.Lock()
		defer mu.Unlock()
		if len(auths) == 0 {
			t.Fatal("upstream received no request")
		}
		if got, want := auths[len(auths)-1], "Bearer k1"; got != want {
			t.Fatalf("Authorization = %q, want %q (raw comma list leaked: %q)", got, want, got)
		}
	})

	t.Run("single-key provider unchanged", func(t *testing.T) {
		pm, err := provider.NewProviderManager(map[string]provider.ProviderConfig{
			"q": {BaseURL: srv.URL, APIKey: "k1"},
		}, nil)
		if err != nil {
			t.Fatalf("NewProviderManager() error = %v", err)
		}
		def := config.ProviderDef{BaseURL: srv.URL, APIKey: "k1", Protocol: config.ProtocolOpenAIChat}
		c := newBootChatClient(pm, def, "q", srv.Client())
		if _, err := c.CreateChat(context.Background(), chatReq()); err != nil {
			t.Fatalf("CreateChat() error = %v", err)
		}
		mu.Lock()
		defer mu.Unlock()
		if len(auths) == 0 {
			t.Fatal("upstream received no request")
		}
		if got, want := auths[len(auths)-1], "Bearer k1"; got != want {
			t.Fatalf("Authorization = %q, want %q", got, want)
		}
	})
}
