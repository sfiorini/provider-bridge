package provider

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"
	"unsafe"

	"providerbridge/internal/config"
	"providerbridge/internal/protocol/anthropic"
	"providerbridge/internal/protocol/chat"
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

	// Reload with a SHORTER key list: out-of-range index 2 clamps to 0 —
	// the same rule SetKeyRotationStore applies to a persisted index on
	// restart, so reload and restart behave identically.
	if err := pm.Reload(newCfg("k1,k2")); err != nil {
		t.Fatalf("Reload() error = %v", err)
	}
	if idx := pm.ActiveKeyIndex("p"); idx != 0 {
		t.Fatalf("ActiveKeyIndex(p) after shrink reload = %d, want 0 (clamped, restart-consistent)", idx)
	}

	// Advance within the 2-key list, reload unchanged: index survives.
	if !pm.advanceKeyIndex("p", 0, 1) {
		t.Fatal("advanceKeyIndex(p,0,1) = false, want true")
	}
	if err := pm.Reload(newCfg("k1,k2")); err != nil {
		t.Fatalf("Reload() error = %v", err)
	}
	if idx := pm.ActiveKeyIndex("p"); idx != 1 {
		t.Fatalf("ActiveKeyIndex(p) after unchanged reload = %d, want 1", idx)
	}
}

// TestProviderAPIKeyFailsClosedOnEmptyKeySet checks that ProviderAPIKey never
// falls back to the raw config APIKey string when a provider has no parsed
// keys: the raw value may be a comma-separated list, and putting it into an
// Authorization header would leak every key.
func TestProviderAPIKeyFailsClosedOnEmptyKeySet(t *testing.T) {
	pm, err := NewProviderManager(map[string]ProviderConfig{
		"p": {BaseURL: "https://p.example", APIKey: "k1"},
	}, nil)
	if err != nil {
		t.Fatalf("NewProviderManager() error = %v", err)
	}
	// Simulate an inconsistent manager state (provider configured but no
	// parsed keys): ProviderAPIKey must fail closed with an empty string,
	// not return the raw cfg.APIKey list.
	pm.mu.Lock()
	pm.apiKeys["p"] = nil
	pm.mu.Unlock()
	if k := pm.ProviderAPIKey("p"); k != "" {
		t.Fatalf("ProviderAPIKey(p) = %q, want empty (fail closed, never the raw list)", k)
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

// TestRunWithRotation exercises the rotation engine with fake attempt
// functions that record the rotation indexes they were called with.
func TestRunWithRotation(t *testing.T) {
	newPM := func(t *testing.T, apiKey string) *ProviderManager {
		t.Helper()
		pm, err := NewProviderManager(map[string]ProviderConfig{
			"p": {BaseURL: "https://p.example", APIKey: apiKey},
		}, nil)
		if err != nil {
			t.Fatalf("NewProviderManager() error = %v", err)
		}
		return pm
	}

	// The plan's expected call list [0,2] is inconsistent with its binding
	// rotation-order code ([active..n-1,0..] yields sequential indexes 0,1,2);
	// the binding code wins: the fake fails 429 at idx 0 and 1 and succeeds
	// at idx 2, so the recorded calls are [0,1,2] with the active index
	// advanced to 2. The single-429 variant is covered by its own subtest.
	t.Run("429s then success at idx 2 advances active index", func(t *testing.T) {
		pm := newPM(t, "k1,k2,k3")
		var calls []int
		err := pm.runWithRotation(context.Background(), "p", func(idx int) error {
			calls = append(calls, idx)
			if idx < 2 {
				return &anthropic.ProviderError{StatusCode: http.StatusTooManyRequests, Message: "rate limited"}
			}
			return nil
		})
		if err != nil {
			t.Fatalf("runWithRotation() error = %v, want nil", err)
		}
		if want := []int{0, 1, 2}; !slices.Equal(calls, want) {
			t.Fatalf("call indexes = %v, want %v", calls, want)
		}
		if idx := pm.ActiveKeyIndex("p"); idx != 2 {
			t.Fatalf("ActiveKeyIndex(p) = %d, want 2", idx)
		}
	})

	t.Run("429 then immediate success advances one key", func(t *testing.T) {
		pm := newPM(t, "k1,k2,k3")
		var calls []int
		err := pm.runWithRotation(context.Background(), "p", func(idx int) error {
			calls = append(calls, idx)
			if idx == 0 {
				return &anthropic.ProviderError{StatusCode: http.StatusTooManyRequests, Message: "rate limited"}
			}
			return nil
		})
		if err != nil {
			t.Fatalf("runWithRotation() error = %v, want nil", err)
		}
		if want := []int{0, 1}; !slices.Equal(calls, want) {
			t.Fatalf("call indexes = %v, want %v", calls, want)
		}
		if idx := pm.ActiveKeyIndex("p"); idx != 1 {
			t.Fatalf("ActiveKeyIndex(p) = %d, want 1", idx)
		}
	})

	t.Run("non-rotatable error returns immediately", func(t *testing.T) {
		pm := newPM(t, "k1,k2,k3")
		var calls []int
		wantErr := &anthropic.ProviderError{StatusCode: http.StatusInternalServerError, Message: "boom"}
		err := pm.runWithRotation(context.Background(), "p", func(idx int) error {
			calls = append(calls, idx)
			return wantErr
		})
		if err == nil {
			t.Fatal("runWithRotation() = nil, want error")
		}
		if !errors.Is(err, wantErr) {
			t.Fatalf("runWithRotation() error = %v, want the original 500 error", err)
		}
		if want := []int{0}; !slices.Equal(calls, want) {
			t.Fatalf("call indexes = %v, want %v (no rotation)", calls, want)
		}
		if idx := pm.ActiveKeyIndex("p"); idx != 0 {
			t.Fatalf("ActiveKeyIndex(p) = %d, want 0 (unchanged)", idx)
		}
	})

	t.Run("all keys fail with 402 returns first error", func(t *testing.T) {
		pm := newPM(t, "k1,k2,k3")
		var calls []int
		err := pm.runWithRotation(context.Background(), "p", func(idx int) error {
			calls = append(calls, idx)
			return &chat.ProviderError{StatusCode: http.StatusPaymentRequired, Message: fmt.Sprintf("quota exhausted k%d", idx+1)}
		})
		if err == nil {
			t.Fatal("runWithRotation() = nil, want error")
		}
		if err.Error() != "quota exhausted k1" {
			t.Fatalf("runWithRotation() error = %q, want the FIRST attempt's error %q", err.Error(), "quota exhausted k1")
		}
		if want := []int{0, 1, 2}; !slices.Equal(calls, want) {
			t.Fatalf("call indexes = %v, want %v", calls, want)
		}
	})

	t.Run("single key 429 no rotation machinery", func(t *testing.T) {
		pm := newPM(t, "k1")
		var calls []int
		err := pm.runWithRotation(context.Background(), "p", func(idx int) error {
			calls = append(calls, idx)
			return &anthropic.ProviderError{StatusCode: http.StatusTooManyRequests, Message: "rate limited"}
		})
		if err == nil {
			t.Fatal("runWithRotation() = nil, want error")
		}
		if err.Error() != "rate limited" {
			t.Fatalf("runWithRotation() error = %q, want %q", err.Error(), "rate limited")
		}
		if want := []int{0}; !slices.Equal(calls, want) {
			t.Fatalf("call indexes = %v, want %v", calls, want)
		}
	})

	t.Run("context cancelled between attempts", func(t *testing.T) {
		pm := newPM(t, "k1,k2,k3")
		ctx, cancel := context.WithCancel(context.Background())
		var calls []int
		err := pm.runWithRotation(ctx, "p", func(idx int) error {
			calls = append(calls, idx)
			cancel()
			return &anthropic.ProviderError{StatusCode: http.StatusTooManyRequests, Message: "rate limited"}
		})
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("runWithRotation() error = %v, want context.Canceled", err)
		}
		if want := []int{0}; !slices.Equal(calls, want) {
			t.Fatalf("call indexes = %v, want %v (no further attempt after cancel)", calls, want)
		}
	})

	t.Run("unknown provider", func(t *testing.T) {
		pm := newPM(t, "k1,k2")
		err := pm.runWithRotation(context.Background(), "missing", func(idx int) error { return nil })
		if err == nil || !strings.Contains(err.Error(), "not found") {
			t.Fatalf("runWithRotation(missing) = %v, want provider-not-found error", err)
		}
	})
}

// TestIsRotatableError checks the 429/402-only rotation trigger rule.
func TestIsRotatableError(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want bool
	}{
		{"anthropic 429", &anthropic.ProviderError{StatusCode: http.StatusTooManyRequests}, true},
		{"anthropic 402", &anthropic.ProviderError{StatusCode: http.StatusPaymentRequired}, true},
		{"anthropic 500", &anthropic.ProviderError{StatusCode: http.StatusInternalServerError}, false},
		{"anthropic 401", &anthropic.ProviderError{StatusCode: http.StatusUnauthorized}, false},
		{"chat 429", &chat.ProviderError{StatusCode: http.StatusTooManyRequests}, true},
		{"chat 402", &chat.ProviderError{StatusCode: http.StatusPaymentRequired}, true},
		{"chat 500", &chat.ProviderError{StatusCode: http.StatusInternalServerError}, false},
		{"plain error", errors.New("network down"), false},
	}
	for _, tc := range cases {
		if got := isRotatableError(tc.err); got != tc.want {
			t.Errorf("%s: isRotatableError() = %v, want %v", tc.name, got, tc.want)
		}
	}
}

// TestRotatingAnthropicClient429 checks that a multi-key provider's
// ProviderClient rotates to the next API key on an HTTP 429.
func TestRotatingAnthropicClient429(t *testing.T) {
	var mu sync.Mutex
	var keys []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		keys = append(keys, r.Header.Get("x-api-key"))
		got := keys[len(keys)-1]
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		if got == "k1" {
			w.WriteHeader(http.StatusTooManyRequests)
			fmt.Fprint(w, `{"error":{"type":"rate_limit_error","message":"rate limited k1"}}`)
			return
		}
		fmt.Fprint(w, `{"id":"msg_1","type":"message","role":"assistant","model":"m","content":[],"stop_reason":"end_turn","usage":{"input_tokens":1,"output_tokens":1}}`)
	}))
	defer srv.Close()

	pm, err := NewProviderManager(map[string]ProviderConfig{
		"p": {BaseURL: srv.URL, APIKey: "k1,k2"},
	}, nil)
	if err != nil {
		t.Fatalf("NewProviderManager() error = %v", err)
	}
	client, err := pm.ClientForKey("p")
	if err != nil {
		t.Fatalf("ClientForKey(p) error = %v", err)
	}
	resp, err := client.CreateMessage(context.Background(), &anthropic.MessageRequest{
		Model: "m", MaxTokens: 1, Messages: []anthropic.Message{{Role: "user", Content: []anthropic.ContentBlock{{Type: "text", Text: "hi"}}}},
	})
	if err != nil {
		t.Fatalf("CreateMessage() error = %v, want nil after rotation", err)
	}
	msg, ok := resp.(anthropic.MessageResponse)
	if !ok {
		t.Fatalf("CreateMessage() response type = %T, want anthropic.MessageResponse", resp)
	}
	if msg.ID != "msg_1" {
		t.Fatalf("CreateMessage() response ID = %q, want %q", msg.ID, "msg_1")
	}
	mu.Lock()
	n := len(keys)
	mu.Unlock()
	if n != 2 {
		t.Fatalf("server saw %d requests, want 2", n)
	}
	if idx := pm.ActiveKeyIndex("p"); idx != 1 {
		t.Fatalf("ActiveKeyIndex(p) = %d, want 1", idx)
	}
}

// TestRotatingAnthropicAllKeysFail checks that when every key returns 429,
// the FIRST (active key's) attempt error is returned.
func TestRotatingAnthropicAllKeysFail(t *testing.T) {
	var mu sync.Mutex
	var keys []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		keys = append(keys, r.Header.Get("x-api-key"))
		got := keys[len(keys)-1]
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusTooManyRequests)
		fmt.Fprintf(w, `{"error":{"type":"rate_limit_error","message":"rate limited %s"}}`, got)
	}))
	defer srv.Close()

	pm, err := NewProviderManager(map[string]ProviderConfig{
		"p": {BaseURL: srv.URL, APIKey: "k1,k2"},
	}, nil)
	if err != nil {
		t.Fatalf("NewProviderManager() error = %v", err)
	}
	client, err := pm.ClientForKey("p")
	if err != nil {
		t.Fatalf("ClientForKey(p) error = %v", err)
	}
	_, err = client.CreateMessage(context.Background(), &anthropic.MessageRequest{
		Model: "m", MaxTokens: 1, Messages: []anthropic.Message{{Role: "user", Content: []anthropic.ContentBlock{{Type: "text", Text: "hi"}}}},
	})
	if err == nil {
		t.Fatal("CreateMessage() = nil, want error")
	}
	if err.Error() != "rate limited k1" {
		t.Fatalf("CreateMessage() error = %q, want the FIRST attempt's error %q", err.Error(), "rate limited k1")
	}
	mu.Lock()
	n := len(keys)
	mu.Unlock()
	if n != 2 {
		t.Fatalf("server saw %d requests, want 2", n)
	}
}

// TestRotatingProviderClientAccessor checks that AnthropicClient exposes
// the ACTIVE plain client (used by ProbeWebSearch; never rotates).
func TestRotatingProviderClientAccessor(t *testing.T) {
	pm, err := NewProviderManager(map[string]ProviderConfig{
		"p": {BaseURL: "https://p.example", APIKey: "k1,k2"},
	}, nil)
	if err != nil {
		t.Fatalf("NewProviderManager() error = %v", err)
	}
	client, err := pm.ClientForKey("p")
	if err != nil {
		t.Fatalf("ClientForKey(p) error = %v", err)
	}
	accessor, ok := client.(AnthropicClientAccessor)
	if !ok {
		t.Fatalf("ClientForKey(p) = %T, want AnthropicClientAccessor", client)
	}
	if accessor.AnthropicClient() != pm.AnthropicClientIndex("p", 0) {
		t.Fatal("AnthropicClient() is not the active (index 0) client")
	}
	if !pm.advanceKeyIndex("p", 0, 1) {
		t.Fatal("advanceKeyIndex(p,0,1) = false, want true")
	}
	if accessor.AnthropicClient() != pm.AnthropicClientIndex("p", 1) {
		t.Fatal("AnthropicClient() did not follow the advanced active index")
	}
}

// TestSingleKeyProviderPlainAdapter checks that single-key providers keep
// the plain anthropicClientAdapter (no rotating types).
func TestSingleKeyProviderPlainAdapter(t *testing.T) {
	pm, err := NewProviderManager(map[string]ProviderConfig{
		"solo": {BaseURL: "https://s.example", APIKey: "solo"},
	}, nil)
	if err != nil {
		t.Fatalf("NewProviderManager() error = %v", err)
	}
	client, err := pm.ClientForKey("solo")
	if err != nil {
		t.Fatalf("ClientForKey(solo) error = %v", err)
	}
	if _, isRotating := client.(*rotatingProviderClient); isRotating {
		t.Fatalf("ClientForKey(solo) = %T, want plain adapter for single key", client)
	}
}

// TestRotatingProviderClientNormalizeError checks that a wrong-typed request
// fails before any rotation.
func TestRotatingProviderClientNormalizeError(t *testing.T) {
	pm, err := NewProviderManager(map[string]ProviderConfig{
		"p": {BaseURL: "https://p.example", APIKey: "k1,k2"},
	}, nil)
	if err != nil {
		t.Fatalf("NewProviderManager() error = %v", err)
	}
	client, err := pm.ClientForKey("p")
	if err != nil {
		t.Fatalf("ClientForKey(p) error = %v", err)
	}
	if _, err := client.CreateMessage(context.Background(), "not a request"); err == nil {
		t.Fatal("CreateMessage(wrong type) = nil, want normalize error")
	}
	if idx := pm.ActiveKeyIndex("p"); idx != 0 {
		t.Fatalf("ActiveKeyIndex(p) = %d, want 0 (no rotation on normalize error)", idx)
	}
}

// fakeAsAnthropicProviderClient is a ProviderClient that does not wrap an
// anthropic client; used to assert the AsAnthropicUpstream error path.
type fakeAsAnthropicProviderClient struct{}

func (fakeAsAnthropicProviderClient) CreateMessage(ctx context.Context, req any) (any, error) {
	return nil, nil
}

func (fakeAsAnthropicProviderClient) StreamMessage(ctx context.Context, req any) (<-chan any, error) {
	return nil, nil
}

// TestAsAnthropicUpstream checks the typed anthropic upstream accessor:
// plain adapters return the wrapped *anthropic.Client, rotating clients
// return their typed rotating client, and unknown types error.
func TestAsAnthropicUpstream(t *testing.T) {
	// Single-key provider: plain adapter returns the same *anthropic.Client.
	pm, err := NewProviderManager(map[string]ProviderConfig{
		"solo": {BaseURL: "https://s.example", APIKey: "solo"},
	}, nil)
	if err != nil {
		t.Fatalf("NewProviderManager() error = %v", err)
	}
	plainClient, err := pm.ClientForKey("solo")
	if err != nil {
		t.Fatalf("ClientForKey(solo) error = %v", err)
	}
	adapter, ok := plainClient.(*anthropicClientAdapter)
	if !ok {
		t.Fatalf("ClientForKey(solo) = %T, want *anthropicClientAdapter", plainClient)
	}
	typed, err := AsAnthropicUpstream(adapter)
	if err != nil {
		t.Fatalf("AsAnthropicUpstream(adapter) error = %v", err)
	}
	if typed != adapter.client {
		t.Fatal("AsAnthropicUpstream(adapter) did not return the wrapped *anthropic.Client (pointer mismatch)")
	}

	// Multi-key provider: rotating client returns its typed rotating client.
	pmm, err := NewProviderManager(map[string]ProviderConfig{
		"p": {BaseURL: "https://p.example", APIKey: "k1,k2"},
	}, nil)
	if err != nil {
		t.Fatalf("NewProviderManager() error = %v", err)
	}
	rotClient, err := pmm.ClientForKey("p")
	if err != nil {
		t.Fatalf("ClientForKey(p) error = %v", err)
	}
	rp, ok := rotClient.(*rotatingProviderClient)
	if !ok {
		t.Fatalf("ClientForKey(p) = %T, want *rotatingProviderClient", rotClient)
	}
	typed, err = AsAnthropicUpstream(rp)
	if err != nil {
		t.Fatalf("AsAnthropicUpstream(rotating) error = %v", err)
	}
	if typed == nil {
		t.Fatal("AsAnthropicUpstream(rotating) = nil, want typed rotating client")
	}
	if typed != rp.typed {
		t.Fatal("AsAnthropicUpstream(rotating) did not return the typed rotating client")
	}

	// Unknown ProviderClient type errors.
	if _, err := AsAnthropicUpstream(fakeAsAnthropicProviderClient{}); err == nil {
		t.Fatal("AsAnthropicUpstream(unknown type) = nil error, want error")
	}
}

// TestSetKeyRotationStoreAfterReload (S-5-2): the manager reloads (as runtime
// does after a config change) BEFORE the rotation store is attached; the
// store then loads the persisted index, and a later advance persists the
// (provider, index) pair to the fake store.
func TestSetKeyRotationStoreAfterReload(t *testing.T) {
	pm, err := NewProviderManager(map[string]ProviderConfig{
		"p": {BaseURL: "https://p.example", APIKey: "k1,k2,k3"},
	}, nil)
	if err != nil {
		t.Fatalf("NewProviderManager() error = %v", err)
	}

	// Reload the manager as runtime does after a config change.
	if err := pm.Reload(config.ProviderConfig{
		Providers: map[string]config.ProviderDef{
			"p": {BaseURL: "https://p.example", APIKey: "k1,k2,k3"},
		},
	}); err != nil {
		t.Fatalf("Reload() error = %v", err)
	}

	fake := &fakeRotationStore{loaded: map[string]int{"p": 1}}
	pm.SetKeyRotationStore(fake)
	if idx := pm.ActiveKeyIndex("p"); idx != 1 {
		t.Fatalf("ActiveKeyIndex(p) = %d, want 1 (loaded from store after reload)", idx)
	}

	// Advance: the fake store must record the (provider, index) pair.
	if !pm.advanceKeyIndex("p", 1, 2) {
		t.Fatal("advanceKeyIndex(p,1,2) = false, want true")
	}
	got := false
	for deadline := time.Now().Add(time.Second); time.Now().Before(deadline); {
		for _, c := range fake.recorded() {
			if c == "p:2" {
				got = true
			}
		}
		if got {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	if !got {
		t.Fatalf("fake store never received (p,2); calls = %v", fake.recorded())
	}
}

// TestIsRotatableStatus checks the shared 429/402 rotation policy used by
// isRotatableError and the raw passthrough loop.
func TestIsRotatableStatus(t *testing.T) {
	for _, tc := range []struct {
		code int
		want bool
	}{
		{http.StatusTooManyRequests, true},
		{http.StatusPaymentRequired, true},
		{http.StatusBadRequest, false},
		{http.StatusUnauthorized, false},
		{http.StatusInternalServerError, false},
		{http.StatusOK, false},
	} {
		if got := IsRotatableStatus(tc.code); got != tc.want {
			t.Errorf("IsRotatableStatus(%d) = %v, want %v", tc.code, got, tc.want)
		}
	}
}

// gatedRotationStore blocks the FIRST SetProviderKeyIndex call until
// released, letting tests force out-of-order writer completion.
type gatedRotationStore struct {
	mu           sync.Mutex
	loaded       map[string]int
	calls        []string
	firstEntered chan struct{} // closed exactly once when the first call enters
	entered      bool          // guards firstEntered so it is never reassigned (race-safe)
	release      chan struct{}
}

func (s *gatedRotationStore) LoadProviderKeyIndexes(ctx context.Context) (map[string]int, error) {
	return s.loaded, nil
}

func (s *gatedRotationStore) SetProviderKeyIndex(ctx context.Context, providerKey string, idx int) error {
	s.mu.Lock()
	if !s.entered {
		s.entered = true
		close(s.firstEntered)
		s.mu.Unlock()
		<-s.release
	} else {
		s.mu.Unlock()
	}
	s.mu.Lock()
	s.calls = append(s.calls, fmt.Sprintf("%s:%d", providerKey, idx))
	s.mu.Unlock()
	return nil
}

func (s *gatedRotationStore) recorded() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.calls...)
}

// TestAdvanceKeyIndexPersistLatestWins reproduces the F9 race: the writer
// for the FIRST advance (0->1) is held inside the store while the second
// advance (1->2) fires. The persist must be serialized per provider and
// re-read the live index before writing, so the store ends with the
// LATEST index (p:2), never the stale one (poll <= 1s).
func TestAdvanceKeyIndexPersistLatestWins(t *testing.T) {
	pm, err := NewProviderManager(map[string]ProviderConfig{
		"p": {BaseURL: "https://p.example", APIKey: "k1,k2,k3"},
	}, nil)
	if err != nil {
		t.Fatalf("NewProviderManager() error = %v", err)
	}
	store := &gatedRotationStore{
		loaded:       map[string]int{},
		firstEntered: make(chan struct{}),
		release:      make(chan struct{}),
	}
	pm.SetKeyRotationStore(store)

	// First advance: its persist writer must reach the store and block.
	if !pm.advanceKeyIndex("p", 0, 1) {
		t.Fatal("advanceKeyIndex(p,0,1) = false, want true")
	}
	select {
	case <-store.firstEntered:
	case <-time.After(time.Second):
		t.Fatal("first persist never reached the store")
	}

	// Second advance while the first writer is stuck inside the store. In
	// the racy implementation the second writer completes NOW (before the
	// first); in the serialized implementation it queues behind the first.
	// Either way, wait a bounded time for it, then release the first writer.
	if !pm.advanceKeyIndex("p", 1, 2) {
		t.Fatal("advanceKeyIndex(p,1,2) = false, want true")
	}
	sawSecond := false
	for deadline := time.Now().Add(500 * time.Millisecond); time.Now().Before(deadline); {
		for _, c := range store.recorded() {
			if c == "p:2" {
				sawSecond = true
			}
		}
		if sawSecond {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	close(store.release)

	// Poll up to 1s for both persist writes to land.
	var calls []string
	for deadline := time.Now().Add(time.Second); time.Now().Before(deadline); {
		calls = store.recorded()
		if len(calls) >= 2 {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	if len(calls) != 2 {
		t.Fatalf("expected 2 persist calls, got %v", calls)
	}
	if last := calls[len(calls)-1]; last != "p:2" {
		t.Fatalf("final persisted index = %s, want p:2 (latest wins)", last)
	}
	if idx := pm.ActiveKeyIndex("p"); idx != 2 {
		t.Fatalf("ActiveKeyIndex(p) = %d, want 2", idx)
	}
}

// TestSingleKeyAnthropicClientUsesParsedKey checks that the single-key
// anthropic adapter client is built from the parsed (trimmed) key, not the
// raw config string. Wire-level assertions cannot catch this (net/http trims
// surrounding whitespace from header values), so the test reads the client's
// unexported apiKey field and compares it to ProviderAPIKey (trimmed).
func TestSingleKeyAnthropicClientUsesParsedKey(t *testing.T) {
	pm, err := NewProviderManager(map[string]ProviderConfig{
		"p": {BaseURL: "https://p.example", APIKey: "  padded  "},
	}, nil)
	if err != nil {
		t.Fatalf("NewProviderManager() error = %v", err)
	}
	if k := pm.ProviderAPIKey("p"); k != "padded" {
		t.Fatalf("ProviderAPIKey(p) = %q, want %q (trimmed)", k, "padded")
	}
	client, err := pm.ClientForKey("p")
	if err != nil {
		t.Fatalf("ClientForKey(p) error = %v", err)
	}
	acc, ok := client.(AnthropicClientAccessor)
	if !ok {
		t.Fatalf("single-key client type %T does not implement AnthropicClientAccessor", client)
	}
	field := reflect.ValueOf(acc.AnthropicClient()).Elem().FieldByName("apiKey")
	raw := reflect.NewAt(field.Type(), unsafe.Pointer(field.UnsafeAddr())).Elem().String()
	if raw != "padded" {
		t.Fatalf("anthropic client apiKey = %q, want %q (parsed key, not raw config string)", raw, "padded")
	}
}

// recordingKeyIndexStore is a thread-safe in-memory KeyRotationStore that
// records every persist call, for concurrent persist tests.
type recordingKeyIndexStore struct {
	mu     sync.Mutex
	loaded map[string]int
	calls  []string
}

func (s *recordingKeyIndexStore) LoadProviderKeyIndexes(_ context.Context) (map[string]int, error) {
	return s.loaded, nil
}

func (s *recordingKeyIndexStore) SetProviderKeyIndex(_ context.Context, providerKey string, idx int) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.calls = append(s.calls, fmt.Sprintf("%s:%d", providerKey, idx))
	return nil
}

func (s *recordingKeyIndexStore) recorded() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.calls...)
}

// TestPersistActiveKeyIndexConcurrentManagers reproduces the R1 crash: the
// package-level persist-mutex bookkeeping is shared by EVERY ProviderManager,
// and runtime reloads create NEW manager instances, so persists from
// different managers for the same provider run concurrently. The
// bookkeeping must be concurrency-safe and must serialize persists per
// provider ACROSS managers; without -race the old per-manager map crashed
// the process with a fatal "concurrent map read and map write". The store
// must end up with every persist recorded and a valid index.
func TestPersistActiveKeyIndexConcurrentManagers(t *testing.T) {
	const managers = 64
	const rounds = 16
	store := &recordingKeyIndexStore{loaded: map[string]int{}}
	var wg sync.WaitGroup
	for i := 0; i < managers; i++ {
		pm, err := NewProviderManager(map[string]ProviderConfig{
			"p": {BaseURL: "https://p.example", APIKey: "k1,k2,k3"},
		}, nil)
		if err != nil {
			t.Fatalf("NewProviderManager() error = %v", err)
		}
		pm.SetKeyRotationStore(store)
		wg.Add(1)
		go func(pm *ProviderManager) {
			defer wg.Done()
			for r := 0; r < rounds; r++ {
				pm.persistActiveKeyIndex("p")
			}
		}(pm)
	}
	wg.Wait()
	// persistActiveKeyIndex spawns one goroutine per call; all writers for
	// provider "p" are serialized. Poll until every persist landed.
	want := managers * rounds
	var calls []string
	for deadline := time.Now().Add(2 * time.Second); time.Now().Before(deadline); {
		calls = store.recorded()
		if len(calls) == want {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	if len(calls) != want {
		t.Fatalf("persist calls = %d, want %d (calls=%v)", len(calls), want, calls)
	}
	for _, c := range calls {
		if c != "p:0" {
			t.Fatalf("persisted %q, want p:0 (a valid index for a 3-key provider)", c)
		}
	}
}
