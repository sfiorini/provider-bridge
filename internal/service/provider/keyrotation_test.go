package provider

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"providerbridge/internal/config"
)

func TestAnthropicClientIndexPool(t *testing.T) {
	pm, err := NewProviderManager(map[string]ProviderConfig{
		"p": {BaseURL: "https://p.example", APIKey: "k1,k2"},
	}, nil)
	if err != nil {
		t.Fatalf("NewProviderManager() error = %v", err)
	}

	c0a := pm.AnthropicClientIndex("p", 0)
	if c0a == nil {
		t.Fatal("AnthropicClientIndex(p,0) = nil, want client")
	}
	c0b := pm.AnthropicClientIndex("p", 0)
	if c0a != c0b {
		t.Fatal("AnthropicClientIndex(p,0) not cached: pointers differ")
	}
	c1 := pm.AnthropicClientIndex("p", 1)
	if c1 == nil {
		t.Fatal("AnthropicClientIndex(p,1) = nil, want client")
	}
	if c1 == c0a {
		t.Fatal("AnthropicClientIndex(p,1) equals index-0 client, want distinct")
	}
	// Out-of-range index clamps to 0 and returns the cached idx-0 pointer.
	c7 := pm.AnthropicClientIndex("p", 7)
	if c7 != c0a {
		t.Fatal("AnthropicClientIndex(p,7) does not clamp to index-0 client")
	}
	// Unknown provider yields nil.
	if c := pm.AnthropicClientIndex("missing", 0); c != nil {
		t.Fatal("AnthropicClientIndex(missing,0) = non-nil, want nil")
	}
}

// fakeRotationStore is an in-memory KeyRotationStore recording calls.
type fakeRotationStore struct {
	mu     sync.Mutex
	loaded map[string]int
	calls  []string // "key:idx" per SetProviderKeyIndex call
	setErr error
}

func (f *fakeRotationStore) LoadProviderKeyIndexes(ctx context.Context) (map[string]int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.loaded, nil
}

func (f *fakeRotationStore) SetProviderKeyIndex(ctx context.Context, providerKey string, idx int) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, fmt.Sprintf("%s:%d", providerKey, idx))
	return f.setErr
}

func (f *fakeRotationStore) recorded() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.calls...)
}

func TestSetKeyRotationStoreClamp(t *testing.T) {
	pm, err := NewProviderManager(map[string]ProviderConfig{
		"p": {BaseURL: "https://p.example", APIKey: "k1,k2"},
	}, nil)
	if err != nil {
		t.Fatalf("NewProviderManager() error = %v", err)
	}

	// Persisted index beyond the key count clamps to 0.
	pm.SetKeyRotationStore(&fakeRotationStore{loaded: map[string]int{"p": 5}})
	if idx := pm.ActiveKeyIndex("p"); idx != 0 {
		t.Fatalf("ActiveKeyIndex(p) = %d, want 0 (clamped)", idx)
	}

	// Persisted index within range is honored.
	pm2, err := NewProviderManager(map[string]ProviderConfig{
		"p": {BaseURL: "https://p.example", APIKey: "k1,k2"},
	}, nil)
	if err != nil {
		t.Fatalf("NewProviderManager() error = %v", err)
	}
	pm2.SetKeyRotationStore(&fakeRotationStore{loaded: map[string]int{"p": 1}})
	if idx := pm2.ActiveKeyIndex("p"); idx != 1 {
		t.Fatalf("ActiveKeyIndex(p) = %d, want 1", idx)
	}

	// Nil store leaves in-memory rotation untouched.
	pm.SetKeyRotationStore(nil)
	if idx := pm.ActiveKeyIndex("p"); idx != 0 {
		t.Fatalf("ActiveKeyIndex(p) after nil store = %d, want 0", idx)
	}
}

func TestAdvanceKeyIndexCAS(t *testing.T) {
	pm, err := NewProviderManager(map[string]ProviderConfig{
		"p": {BaseURL: "https://p.example", APIKey: "k1,k2,k3"},
	}, nil)
	if err != nil {
		t.Fatalf("NewProviderManager() error = %v", err)
	}
	store := &fakeRotationStore{}
	pm.SetKeyRotationStore(store)

	// Successful CAS advance 0 -> 1.
	if !pm.advanceKeyIndex("p", 0, 1) {
		t.Fatal("advanceKeyIndex(p,0,1) = false, want true")
	}
	if idx := pm.ActiveKeyIndex("p"); idx != 1 {
		t.Fatalf("ActiveKeyIndex(p) = %d, want 1 after advance", idx)
	}
	// Async persist: poll up to 1s for the store to receive (p,1).
	got := false
	for deadline := time.Now().Add(time.Second); time.Now().Before(deadline); {
		for _, c := range store.recorded() {
			if c == "p:1" {
				got = true
			}
		}
		if got {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	if !got {
		t.Fatalf("store never received (p,1); calls = %v", store.recorded())
	}

	// Second advance from stale index 0 must fail (current is 1).
	if pm.advanceKeyIndex("p", 0, 2) {
		t.Fatal("advanceKeyIndex(p,0,2) = true, want false (stale CAS)")
	}
	if idx := pm.ActiveKeyIndex("p"); idx != 1 {
		t.Fatalf("ActiveKeyIndex(p) = %d, want 1 (unchanged by failed CAS)", idx)
	}
}

func TestReloadKeepsRotation(t *testing.T) {
	newCfg := func(apiKey string) config.ProviderConfig {
		return config.ProviderConfig{
			Providers: map[string]config.ProviderDef{
				"p": {BaseURL: "https://p.example", APIKey: apiKey},
			},
		}
	}

	pm, err := NewProviderManager(map[string]ProviderConfig{
		"p": {BaseURL: "https://p.example", APIKey: "k1,k2,k3"},
	}, nil)
	if err != nil {
		t.Fatalf("NewProviderManager() error = %v", err)
	}
	if !pm.advanceKeyIndex("p", 0, 2) {
		t.Fatal("advanceKeyIndex(p,0,2) = false, want true")
	}

	// Reload with a SHORTER key list: index 2 clamps to len-1 = 1.
	if err := pm.Reload(newCfg("k1,k2")); err != nil {
		t.Fatalf("Reload() error = %v", err)
	}
	if idx := pm.ActiveKeyIndex("p"); idx != 1 {
		t.Fatalf("ActiveKeyIndex(p) after shrink reload = %d, want 1 (clamped)", idx)
	}

	// Advance back to 2 within the 3-key list, reload unchanged: index survives.
	if !pm.advanceKeyIndex("p", 1, 2) {
		t.Fatal("advanceKeyIndex(p,1,2) = false, want true")
	}
	if err := pm.Reload(newCfg("k1,k2,k3")); err != nil {
		t.Fatalf("Reload() error = %v", err)
	}
	if idx := pm.ActiveKeyIndex("p"); idx != 2 {
		t.Fatalf("ActiveKeyIndex(p) after unchanged reload = %d, want 2", idx)
	}
}

func TestProviderManagerKeyParsing(t *testing.T) {
	// Multi-key provider: keys parse, active defaults to index 0.
	pm, err := NewProviderManager(map[string]ProviderConfig{
		"p": {BaseURL: "https://p.example", APIKey: "k1,k2"},
	}, nil)
	if err != nil {
		t.Fatalf("NewProviderManager() multi-key error = %v", err)
	}
	if n := pm.ProviderKeyCount("p"); n != 2 {
		t.Fatalf("ProviderKeyCount(\"p) = %d, want 2", n)
	}
	if k := pm.ProviderAPIKeyIndex("p", 1); k != "k2" {
		t.Fatalf("ProviderAPIKeyIndex(\"p,'1) = %q, want %q", k, "k2")
	}
	if k := pm.ProviderAPIKey("p"); k != "k1" {
		t.Fatalf("ProviderAPIKey(\"p) = %q, want %q (active key, not raw list)", k, "k1")
	}
	if idx := pm.ActiveKeyIndex("p"); idx != 0 {
		t.Fatalf("ActiveKeyIndex(\"p) = %d, want 0", idx)
	}

	// Single-key provider: behavior identical to before.
	pm, err = NewProviderManager(map[string]ProviderConfig{
		"solo": {BaseURL: "https://s.example", APIKey: "solo"},
	}, nil)
	if err != nil {
		t.Fatalf("NewProviderManager() single-key error = %v", err)
	}
	if n := pm.ProviderKeyCount("solo"); n != 1 {
		t.Fatalf("ProviderKeyCount(\"solo) = %d, want 1", n)
	}
	if k := pm.ProviderAPIKey("solo"); k != "solo" {
		t.Fatalf("ProviderAPIKey(\"solo) = %q, want %q", k, "solo")
	}

	// Whitespace-only key list is rejected at build time.
	_, err = NewProviderManager(map[string]ProviderConfig{
		"bad": {BaseURL: "https://b.example", APIKey: ","},
	}, nil)
	if err == nil {
		t.Fatal("NewProviderManager() with APIKey ,'\" should fail")
	}
	if !strings.Contains(err.Error(), "api_key is required") {
		t.Fatalf("NewProviderManager() error = %v, want it to contain %q", err, "api_key is required")
	}

	// Unknown provider: count 0, index accessor "".
	if n := pm.ProviderKeyCount("missing"); n != 0 {
		t.Fatalf("ProviderKeyCount(\"missing) = %d, want 0", n)
	}
	if k := pm.ProviderAPIKeyIndex("missing", 0); k != "" {
		t.Fatalf("ProviderAPIKeyIndex(\"missing,'0) = %q, want empty", k)
	}
	if k := pm.ProviderAPIKey("missing"); k != "" {
		t.Fatalf("ProviderAPIKey(\"missing) = %q, want empty", k)
	}
	if idx := pm.ActiveKeyIndex("missing"); idx != 0 {
		t.Fatalf("ActiveKeyIndex(\"missing) = %d, want 0", idx)
	}
}
