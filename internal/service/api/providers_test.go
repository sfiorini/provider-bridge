package api

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

// TestHandleTestProviderUsesActiveKey verifies that the test-probe request
// sent by POST /providers/{key}/test carries the provider's ACTIVE key from
// the manager, not the raw comma-separated def.APIKey (F3).
func TestHandleTestProviderUsesActiveKey(t *testing.T) {
	var gotAPIKey string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAPIKey = r.Header.Get("x-api-key")
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`{"id":"msg_1","type":"message","role":"assistant","model":"claude-3-haiku-20240307","content":[{"type":"text","text":"hi"}],"stop_reason":"end_turn","usage":{"input_tokens":1,"output_tokens":1}}`))
	}))
	t.Cleanup(upstream.Close)

	f := newFixture(t)

	// Multi-key provider: active key (index 0) is "k1", the raw def.APIKey
	// value is "k1,k2". Reload so the snapshot carries a real ProviderMgr.
	snap := f.rt.Current()
	cfg := snap.Config
	def := cfg.ProviderDefs["anthropic"]
	def.BaseURL = upstream.URL
	def.APIKey = "k1,k2"
	cfg.ProviderDefs["anthropic"] = def
	if err := f.rt.Reload(cfg); err != nil {
		t.Fatalf("rt.Reload() error = %v", err)
	}
	if f.rt.Current().ProviderMgr == nil {
		t.Fatal("ProviderMgr is nil after reload")
	}

	resp := f.request("POST", "/providers/anthropic/test", nil)
	if resp.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", resp.Code, resp.Body.String())
	}

	if gotAPIKey != "k1" {
		t.Fatalf("test probe sent x-api-key %q, want active key %q (raw def.APIKey is %q)", gotAPIKey, "k1", "k1,k2")
	}
}
