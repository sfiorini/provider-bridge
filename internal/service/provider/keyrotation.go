// Package provider manages multiple upstream LLM providers and routes
// requests to the correct provider based on the requested model.

package provider

import "context"

// KeyRotationStore persists the active API-key rotation index per provider.
// Implementations must be safe for concurrent use; SetProviderKeyIndex is a
// single-row upsert. Persistence is best-effort and asynchronous — failures
// are logged, never propagated to request handling.
type KeyRotationStore interface {
	LoadProviderKeyIndexes(ctx context.Context) (map[string]int, error)
	SetProviderKeyIndex(ctx context.Context, providerKey string, idx int) error
}
