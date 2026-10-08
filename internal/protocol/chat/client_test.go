package chat

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
)

// TestProviderErrorCreateChat verifies that CreateChat returns a typed
// *ProviderError on non-2xx HTTP statuses, with the same opaque message
// text previously produced via fmt.Errorf.
func TestProviderErrorCreateChat(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("x-request-id", "req-123")
		w.WriteHeader(http.StatusTooManyRequests)
		if _, err := w.Write([]byte("rate limited")); err != nil {
			t.Errorf("write response body: %v", err)
		}
	}))
	defer srv.Close()

	client := NewClient(ClientConfig{BaseURL: srv.URL})
	resp, err := client.CreateChat(context.Background(), &ChatRequest{Model: "test-model"})
	if err == nil {
		t.Fatalf("expected error, got response: %+v", resp)
	}

	pe, ok := IsProviderError(err)
	if !ok {
		t.Fatalf("expected *ProviderError, got %T: %v", err, err)
	}
	if pe.StatusCode != http.StatusTooManyRequests {
		t.Errorf("StatusCode = %d, want %d", pe.StatusCode, http.StatusTooManyRequests)
	}
	if pe.RequestID != "req-123" {
		t.Errorf("RequestID = %q, want %q", pe.RequestID, "req-123")
	}
	wantMsg := "chat API error: status=429 body=rate limited"
	if got := err.Error(); got != wantMsg {
		t.Errorf("Error() = %q, want %q", got, wantMsg)
	}
}

// TestProviderErrorStreamChat verifies that StreamChat returns a typed
// *ProviderError on non-2xx HTTP statuses, with the same opaque message
// text previously produced via fmt.Errorf.
func TestProviderErrorStreamChat(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusPaymentRequired)
		if _, err := w.Write([]byte("quota exceeded")); err != nil {
			t.Errorf("write response body: %v", err)
		}
	}))
	defer srv.Close()

	client := NewClient(ClientConfig{BaseURL: srv.URL})
	ch, err := client.StreamChat(context.Background(), &ChatRequest{Model: "test-model"})
	if err == nil {
		t.Fatalf("expected error, got channel: %v", ch)
	}

	pe, ok := IsProviderError(err)
	if !ok {
		t.Fatalf("expected *ProviderError, got %T: %v", err, err)
	}
	if pe.StatusCode != http.StatusPaymentRequired {
		t.Errorf("StatusCode = %d, want %d", pe.StatusCode, http.StatusPaymentRequired)
	}
	wantMsg := "chat API stream error: status=402 body=quota exceeded"
	if got := err.Error(); got != wantMsg {
		t.Errorf("Error() = %q, want %q", got, wantMsg)
	}
}
