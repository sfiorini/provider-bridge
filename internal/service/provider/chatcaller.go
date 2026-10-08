// Package provider manages multiple upstream LLM providers and routes
// requests to the correct provider based on the requested model.

package provider

import (
	"context"

	"providerbridge/internal/protocol/chat"
)

// ChatCaller is the chat upstream call surface used by the server.
// *chat.Client satisfies it natively; rotatingChatClient adds per-key
// rotation for providers configured with multiple API keys.
type ChatCaller interface {
	CreateChat(ctx context.Context, req *chat.ChatRequest) (*chat.ChatResponse, error)
	StreamChat(ctx context.Context, req *chat.ChatRequest) (<-chan chat.ChatStreamChunk, error)
}

var _ ChatCaller = (*chat.Client)(nil)

// rotatingChatClient rotates CreateChat/StreamChat across the provider's
// API keys. Rotation wraps the individual call: the identical in-memory
// request is re-sent verbatim with the next key.
type rotatingChatClient struct {
	pm             *ProviderManager
	providerKey    string
	clientForIndex func(int) *chat.Client
}

// NewRotatingChatClient builds a rotating ChatCaller. clientForIndex must
// return the *chat.Client holding the API key at rotation index idx.
func NewRotatingChatClient(pm *ProviderManager, providerKey string, clientForIndex func(int) *chat.Client) ChatCaller {
	return &rotatingChatClient{pm: pm, providerKey: providerKey, clientForIndex: clientForIndex}
}

func (c *rotatingChatClient) CreateChat(ctx context.Context, req *chat.ChatRequest) (*chat.ChatResponse, error) {
	var resp *chat.ChatResponse
	err := c.pm.runWithRotation(ctx, c.providerKey, func(idx int) error {
		var err error
		resp, err = c.clientForIndex(idx).CreateChat(ctx, req)
		return err
	})
	if err != nil {
		return nil, err
	}
	return resp, nil
}

func (c *rotatingChatClient) StreamChat(ctx context.Context, req *chat.ChatRequest) (<-chan chat.ChatStreamChunk, error) {
	var stream <-chan chat.ChatStreamChunk
	err := c.pm.runWithRotation(ctx, c.providerKey, func(idx int) error {
		var err error
		stream, err = c.clientForIndex(idx).StreamChat(ctx, req)
		return err
	})
	if err != nil {
		return nil, err
	}
	return stream, nil
}
