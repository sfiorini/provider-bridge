package provider

import (
	"strings"
	"testing"
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
