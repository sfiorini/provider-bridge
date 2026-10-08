// Package chat implements the OpenAI Chat Completions ProviderAdapter for ProviderBridge.

package chat

import (
	"errors"
	"net/http"
)

// ProviderError is the typed error returned by CreateChat/StreamChat on
// non-2xx HTTP statuses. It mirrors anthropic.ProviderError so key-rotation
// logic can detect rate/quota failures (HTTP 429/402) without string
// matching. Message keeps the exact opaque text previously returned via
// fmt.Errorf, so logs and consumer error surfaces are unchanged.
type ProviderError struct {
	StatusCode int
	Type       string
	Message    string
	RequestID  string
}

// Error returns the opaque message text, falling back to the HTTP status
// phrase when the response body was empty.
func (err *ProviderError) Error() string {
	if err.Message != "" {
		return err.Message
	}
	return http.StatusText(err.StatusCode)
}

// IsProviderError reports whether err is a *ProviderError.
func IsProviderError(err error) (*ProviderError, bool) {
	var pe *ProviderError
	if errors.As(err, &pe) {
		return pe, true
	}
	return nil, false
}
