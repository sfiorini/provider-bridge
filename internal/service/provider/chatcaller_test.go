package provider

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"providerbridge/internal/protocol/chat"
)

// TestRotatingChatClient429 checks that a rotating ChatCaller retries with
// the next API key on an HTTP 429 and advances the active index.
func TestRotatingChatClient429(t *testing.T) {
	var mu sync.Mutex
	var auths []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		auths = append(auths, r.Header.Get("Authorization"))
		got := auths[len(auths)-1]
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		if got == "Bearer k1" {
			w.WriteHeader(http.StatusTooManyRequests)
			fmt.Fprint(w, `{"error":{"message":"rate limited k1","type":"rate_limit_error"}}`)
			return
		}
		fmt.Fprint(w, `{"id":"chatcmpl-1","object":"chat.completion","created":1,"model":"m","choices":[{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}]}`)
	}))
	defer srv.Close()

	pm, err := NewProviderManager(map[string]ProviderConfig{
		"p": {BaseURL: srv.URL, APIKey: "k1,k2"},
	}, nil)
	if err != nil {
		t.Fatalf("NewProviderManager() error = %v", err)
	}
	caller := NewRotatingChatClient(pm, "p", func(idx int) *chat.Client {
		return chat.NewClient(chat.ClientConfig{BaseURL: srv.URL, APIKey: pm.ProviderAPIKeyIndex("p", idx)})
	})
	req := &chat.ChatRequest{Model: "m", Messages: []chat.ChatMessage{{Role: "user", Content: "hi"}}}
	resp, err := caller.CreateChat(context.Background(), req)
	if err != nil {
		t.Fatalf("CreateChat() error = %v, want nil after rotation", err)
	}
	if resp.ID != "chatcmpl-1" {
		t.Fatalf("CreateChat() response ID = %q, want %q", resp.ID, "chatcmpl-1")
	}
	mu.Lock()
	n := len(auths)
	mu.Unlock()
	if n != 2 {
		t.Fatalf("server saw %d requests, want 2", n)
	}
	if idx := pm.ActiveKeyIndex("p"); idx != 1 {
		t.Fatalf("ActiveKeyIndex(p) = %d, want 1", idx)
	}
}

// TestRotatingChatClientAllKeysFail checks that when every key returns 429,
// the FIRST (active key's) attempt error is returned.
func TestRotatingChatClientAllKeysFail(t *testing.T) {
	var mu sync.Mutex
	var auths []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		auths = append(auths, r.Header.Get("Authorization"))
		got := auths[len(auths)-1]
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusTooManyRequests)
		fmt.Fprintf(w, `rate limited %s`, got)
	}))
	defer srv.Close()

	pm, err := NewProviderManager(map[string]ProviderConfig{
		"p": {BaseURL: srv.URL, APIKey: "k1,k2"},
	}, nil)
	if err != nil {
		t.Fatalf("NewProviderManager() error = %v", err)
	}
	caller := NewRotatingChatClient(pm, "p", func(idx int) *chat.Client {
		return chat.NewClient(chat.ClientConfig{BaseURL: srv.URL, APIKey: pm.ProviderAPIKeyIndex("p", idx)})
	})
	req := &chat.ChatRequest{Model: "m", Messages: []chat.ChatMessage{{Role: "user", Content: "hi"}}}
	_, err = caller.CreateChat(context.Background(), req)
	if err == nil {
		t.Fatal("CreateChat() = nil, want error")
	}
	var pe *chat.ProviderError
	if !errors.As(err, &pe) {
		t.Fatalf("CreateChat() error = %T, want *chat.ProviderError", err)
	}
	if !strings.Contains(pe.Message, "Bearer k1") || strings.Contains(pe.Message, "Bearer k2") {
		t.Fatalf("CreateChat() error message = %q, want the FIRST attempt's (k1) error", pe.Message)
	}
	mu.Lock()
	n := len(auths)
	mu.Unlock()
	if n != 2 {
		t.Fatalf("server saw %d requests, want 2", n)
	}
}

// TestRotatingChatClientStream429 checks that StreamChat rotates on a 429
// before any chunk is emitted.
func TestRotatingChatClientStream429(t *testing.T) {
	var mu sync.Mutex
	var auths []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		auths = append(auths, r.Header.Get("Authorization"))
		got := auths[len(auths)-1]
		mu.Unlock()
		if got == "Bearer k1" {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusTooManyRequests)
			fmt.Fprint(w, `{"error":{"message":"rate limited k1","type":"rate_limit_error"}}`)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, "data: {\"id\":\"chatcmpl-s\",\"object\":\"chat.completion.chunk\",\"created\":1,\"model\":\"m\",\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\"}}]}\n\n")
		fmt.Fprint(w, "\n")
	}))
	defer srv.Close()

	pm, err := NewProviderManager(map[string]ProviderConfig{
		"p": {BaseURL: srv.URL, APIKey: "k1,k2"},
	}, nil)
	if err != nil {
		t.Fatalf("NewProviderManager() error = %v", err)
	}
	caller := NewRotatingChatClient(pm, "p", func(idx int) *chat.Client {
		return chat.NewClient(chat.ClientConfig{BaseURL: srv.URL, APIKey: pm.ProviderAPIKeyIndex("p", idx)})
	})
	req := &chat.ChatRequest{Model: "m", Messages: []chat.ChatMessage{{Role: "user", Content: "hi"}}}
	ch, err := caller.StreamChat(context.Background(), req)
	if err != nil {
		t.Fatalf("StreamChat() error = %v, want nil after rotation", err)
	}
	select {
	case _, ok := <-ch:
		if !ok {
			t.Fatal("StreamChat() channel closed without any chunk")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("StreamChat() channel produced no chunk within timeout")
	}
	mu.Lock()
	n := len(auths)
	mu.Unlock()
	if n != 2 {
		t.Fatalf("server saw %d requests, want 2", n)
	}
	if idx := pm.ActiveKeyIndex("p"); idx != 1 {
		t.Fatalf("ActiveKeyIndex(p) = %d, want 1", idx)
	}
}

// TestChatCallerPlainChatClient checks that a plain *chat.Client satisfies
// ChatCaller natively and flows through the interface without rotation.
func TestChatCallerPlainChatClient(t *testing.T) {
	var mu sync.Mutex
	var n int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		n++
		mu.Unlock()
		fmt.Fprint(w, `{"id":"chatcmpl-2","object":"chat.completion","created":1,"model":"m","choices":[{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}]}`)
	}))
	defer srv.Close()

	var caller ChatCaller = chat.NewClient(chat.ClientConfig{BaseURL: srv.URL, APIKey: "solo"})
	req := &chat.ChatRequest{Model: "m", Messages: []chat.ChatMessage{{Role: "user", Content: "hi"}}}
	resp, err := caller.CreateChat(context.Background(), req)
	if err != nil {
		t.Fatalf("CreateChat() error = %v", err)
	}
	if resp.ID != "chatcmpl-2" {
		t.Fatalf("CreateChat() response ID = %q, want %q", resp.ID, "chatcmpl-2")
	}
	mu.Lock()
	count := n
	mu.Unlock()
	if count != 1 {
		t.Fatalf("server saw %d requests, want 1", count)
	}
}
