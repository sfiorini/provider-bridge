// Package provider manages multiple upstream LLM providers and routes
// requests to the correct provider based on the requested model.
package provider

import (
	"context"
	"fmt"

	"providerbridge/internal/protocol/anthropic"
)

// ProviderClient is the interface for upstream provider API clients.
// Replaces *anthropic.Client in ProviderCandidate, ClientForKey, ClientFor,
// and all callers.
//
// Using any for request/response types avoids protocol-specific coupling
// (anthropic.MessageRequest etc.). Provider adapters handle type assertions
// internally.
type ProviderClient interface {
	// CreateMessage sends a synchronous request to the upstream provider.
	CreateMessage(ctx context.Context, req any) (any, error)

	// StreamMessage sends a streaming request to the upstream provider.
	StreamMessage(ctx context.Context, req any) (<-chan any, error)
}

// AnthropicClientAccessor is implemented by ProviderClient implementations
// that wrap an *anthropic.Client. Callers that need the concrete client for
// protocol-specific operations (e.g., web search probing) can use this
// interface to access the underlying client.
type AnthropicClientAccessor interface {
	AnthropicClient() *anthropic.Client
}

// AnthropicUpstreamClient is the typed anthropic call surface: both
// *anthropic.Client and the rotating typed client satisfy it.
type AnthropicUpstreamClient interface {
	CreateMessage(ctx context.Context, req anthropic.MessageRequest) (anthropic.MessageResponse, error)
	StreamMessage(ctx context.Context, req anthropic.MessageRequest) (anthropic.Stream, error)
}

// AsAnthropicUpstream returns the typed anthropic client behind a
// ProviderClient: the rotating typed client for multi-key providers, the
// plain *anthropic.Client for single-key providers. Rotation happens inside
// the returned client's calls.
func AsAnthropicUpstream(pc ProviderClient) (AnthropicUpstreamClient, error) {
	switch v := pc.(type) {
	case *rotatingProviderClient:
		return v.typed, nil
	case *anthropicClientAdapter:
		return v.client, nil
	default:
		return nil, fmt.Errorf("provider client %T does not support typed anthropic upstream calls", pc)
	}
}
