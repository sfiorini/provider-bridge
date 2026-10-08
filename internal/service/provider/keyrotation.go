// Package provider manages multiple upstream LLM providers and routes
// requests to the correct provider based on the requested model.

package provider

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"time"

	"providerbridge/internal/protocol/anthropic"
	"providerbridge/internal/protocol/chat"
)

// KeyRotationStore persists the active API-key rotation index per provider.
// Implementations must be safe for concurrent use; SetProviderKeyIndex is a
// single-row upsert. Persistence is best-effort and asynchronous — failures
// are logged, never propagated to request handling.
type KeyRotationStore interface {
	LoadProviderKeyIndexes(ctx context.Context) (map[string]int, error)
	SetProviderKeyIndex(ctx context.Context, providerKey string, idx int) error
}

// SetKeyRotationStore attaches the persistence store for key rotation state
// and loads persisted active indexes, clamped to each provider's key count.
// Call once at boot after the manager is built. A nil store or a load error
// leaves in-memory rotation active (indexes start at 0) with a warning.
func (pm *ProviderManager) SetKeyRotationStore(store KeyRotationStore) {
	pm.mu.Lock()
	pm.keyRotationStore = store
	pm.mu.Unlock()
	if store == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	indexes, err := store.LoadProviderKeyIndexes(ctx)
	if err != nil {
		slog.Warn("key rotation state unavailable; starting from index 0", "error", err)
		return
	}
	pm.mu.Lock()
	defer pm.mu.Unlock()
	for key, idx := range indexes {
		n := len(pm.apiKeys[key])
		if n == 0 {
			continue
		}
		if idx < 0 || idx >= n {
			idx = 0
		}
		pm.activeIdx[key] = idx
	}
}

// advanceKeyIndex CAS-advances the active index for providerKey from -> to.
// Returns true when the swap happened. Persist is asynchronous and
// best-effort; a persist failure is logged, never propagated.
func (pm *ProviderManager) advanceKeyIndex(providerKey string, from, to int) bool {
	pm.mu.Lock()
	// An absent entry means index 0 (the map's zero value), so a fresh
	// provider CAS-advances from 0 like an explicitly-set one.
	if cur := pm.activeIdx[providerKey]; cur != from {
		pm.mu.Unlock()
		return false
	}
	pm.activeIdx[providerKey] = to
	store := pm.keyRotationStore
	pm.mu.Unlock()
	slog.Info("active API key advanced", "provider", providerKey, "from_index", from, "to_index", to)
	if store != nil {
		go func() {
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			if err := store.SetProviderKeyIndex(ctx, providerKey, to); err != nil {
				slog.Error("key rotation index persist failed", "provider", providerKey, "index", to, "error", err)
			}
		}()
	}
	return true
}

// AdvanceKeyIndex is the exported form of the CAS advance: it moves the
// active API-key index for providerKey from -> to (persisted
// asynchronously, best-effort) and is used by cross-package call sites
// such as the raw OpenAI Responses passthrough rotation loop.
func (pm *ProviderManager) AdvanceKeyIndex(providerKey string, from, to int) bool {
	return pm.advanceKeyIndex(providerKey, from, to)
}

// runWithRotation executes attempt once per API key of the provider,
// starting at the active index in rotation order [active, active+1, …,
// n-1, 0, …, active-1] (at most n attempts). Only rotatable errors — typed
// provider errors with HTTP status 429 or 402 — trigger an immediate retry
// with the next key (no backoff). Non-rotatable errors and context
// cancellation return immediately. On success at an index different from
// the start index, the active index is CAS-advanced and persisted
// asynchronously. On full rotation failure the FIRST (active key's) attempt
// error is returned.
func (pm *ProviderManager) runWithRotation(ctx context.Context, providerKey string, attempt func(idx int) error) error {
	n := pm.ProviderKeyCount(providerKey)
	if n == 0 {
		return fmt.Errorf("provider %q not found", providerKey)
	}
	start := pm.ActiveKeyIndex(providerKey)
	var firstErr error
	for k := 0; k < n; k++ {
		if err := ctx.Err(); err != nil {
			return err
		}
		idx := (start + k) % n
		err := attempt(idx)
		if err == nil {
			if idx != start {
				pm.advanceKeyIndex(providerKey, start, idx)
			}
			return nil
		}
		if firstErr == nil {
			firstErr = err
		}
		if !isRotatableError(err) {
			return err
		}
		if k < n-1 {
			slog.Warn("upstream key rotation: retrying with next API key",
				"provider", providerKey, "from_index", idx, "to_index", (idx+1)%n)
		}
	}
	return firstErr
}

// isRotatableError reports whether an upstream error should trigger key
// rotation: typed provider errors with HTTP status 429 (rate limited) or
// 402 (payment required / quota exhausted).
func isRotatableError(err error) bool {
	var anthroErr *anthropic.ProviderError
	if errors.As(err, &anthroErr) {
		return anthroErr.StatusCode == http.StatusTooManyRequests ||
			anthroErr.StatusCode == http.StatusPaymentRequired
	}
	var chatErr *chat.ProviderError
	if errors.As(err, &chatErr) {
		return chatErr.StatusCode == http.StatusTooManyRequests ||
			chatErr.StatusCode == http.StatusPaymentRequired
	}
	return false
}

// rotatingAnthropicClient is the typed rotating anthropic client. Rotation
// happens inside CreateMessage/StreamMessage so typed *anthropic.ProviderError
// values reach runWithRotation before any caller-side error wrapping.
type rotatingAnthropicClient struct {
	pm          *ProviderManager
	providerKey string
}

func (c *rotatingAnthropicClient) CreateMessage(ctx context.Context, req anthropic.MessageRequest) (anthropic.MessageResponse, error) {
	var resp anthropic.MessageResponse
	err := c.pm.runWithRotation(ctx, c.providerKey, func(idx int) error {
		var err error
		resp, err = c.pm.AnthropicClientIndex(c.providerKey, idx).CreateMessage(ctx, req)
		return err
	})
	if err != nil {
		return anthropic.MessageResponse{}, err
	}
	return resp, nil
}

func (c *rotatingAnthropicClient) StreamMessage(ctx context.Context, req anthropic.MessageRequest) (anthropic.Stream, error) {
	var stream anthropic.Stream
	err := c.pm.runWithRotation(ctx, c.providerKey, func(idx int) error {
		var err error
		stream, err = c.pm.AnthropicClientIndex(c.providerKey, idx).StreamMessage(ctx, req)
		return err
	})
	if err != nil {
		return nil, err
	}
	return stream, nil
}

// rotatingProviderClient adapts rotatingAnthropicClient to the any-typed
// ProviderClient interface (mirroring anthropicClientAdapter's normalize
// pattern) and exposes the ACTIVE plain client via AnthropicClientAccessor
// (used only by ProbeWebSearch).
type rotatingProviderClient struct {
	typed *rotatingAnthropicClient
}

func (p *rotatingProviderClient) CreateMessage(ctx context.Context, req any) (any, error) {
	msgReq, err := normalizeAnthropicMessageRequest(req)
	if err != nil {
		return nil, err
	}
	return p.typed.CreateMessage(ctx, msgReq)
}

func (p *rotatingProviderClient) StreamMessage(ctx context.Context, req any) (<-chan any, error) {
	msgReq, err := normalizeAnthropicMessageRequest(req)
	if err != nil {
		return nil, err
	}
	stream, err := p.typed.StreamMessage(ctx, msgReq)
	if err != nil {
		return nil, err
	}
	out := make(chan any)
	go func() {
		defer close(out)
		for {
			evt, err := stream.Next()
			if err != nil {
				return
			}
			out <- evt
		}
	}()
	return out, nil
}

func (p *rotatingProviderClient) AnthropicClient() *anthropic.Client {
	return p.typed.pm.AnthropicClientIndex(p.typed.providerKey,
		p.typed.pm.ActiveKeyIndex(p.typed.providerKey))
}

var _ ProviderClient = (*rotatingProviderClient)(nil)
var _ AnthropicClientAccessor = (*rotatingProviderClient)(nil)
