package db

import (
	"context"
	"fmt"
	"log/slog"
)

// Registry manages Provider and Consumer registration, active provider
// selection, table creation, and store binding.
type Registry struct {
	logger    *slog.Logger
	providers []Provider
	consumers []Consumer

	active Provider
	stores map[string]Store // consumer name → Store
}

// NewRegistry creates a new persistence Registry.
func NewRegistry(logger *slog.Logger) *Registry {
	return &Registry{
		logger: logger,
		stores: make(map[string]Store),
	}
}

// RegisterProvider adds a provider to the registry.
func (r *Registry) RegisterProvider(p Provider) {
	if p == nil {
		return
	}
	for _, existing := range r.providers {
		if existing.Name() == p.Name() {
			return // duplicate, silently skip
		}
	}
	r.providers = append(r.providers, p)
}

// RegisterConsumer adds a consumer to the registry.
func (r *Registry) RegisterConsumer(c Consumer) {
	if c == nil {
		return
	}
	for _, existing := range r.consumers {
		if existing.Name() == c.Name() {
			return // duplicate, silently skip
		}
	}
	r.consumers = append(r.consumers, c)
}

// Init selects the active provider, opens the connection, validates consumers,
// creates tables and indexes, and binds stores to consumers.
//
// Error handling strategy:
//   - No consumers: no-op, returns nil
//   - No providers: disables all consumers, logs error, returns nil
//   - Provider selection/Open/Ping/Tables validation failure: returns error (blocking)
//   - Per-consumer table/index creation failure: disables that consumer, continues
//   - Per-consumer BindStore failure: disables that consumer, continues
func (r *Registry) Init(ctx context.Context, activeProviderName string) error {
	// No consumers → nothing to do.
	if len(r.consumers) == 0 {
		return nil
	}

	// Select active provider.
	active, err := r.selectProvider(activeProviderName)
	if err != nil {
		// No providers → disable all consumers gracefully.
		if err == ErrNoProvider {
			r.disableAllConsumers(err)
			return nil
		}
		return err
	}
	r.active = active

	// Open connection.
	if err := active.Open(ctx); err != nil {
		return fmt.Errorf("open provider %q: %w", active.Name(), err)
	}

	// Ping.
	if err := active.Ping(ctx); err != nil {
		active.Close()
		return fmt.Errorf("ping provider %q: %w", active.Name(), err)
	}

	// Validate and initialize each consumer.
	for _, c := range r.consumers {
		if err := r.initConsumer(ctx, c); err != nil {
			// Per-consumer failure: disable this consumer, continue with others.
			r.logError("failed to initialize persistence consumer", "consumer", c.Name(), "error", err)
			c.DisablePersistence(err)
		}
	}
	return nil
}

// selectProvider picks the active provider based on the registered providers
// and the optional activeProviderName parameter.
func (r *Registry) selectProvider(activeName string) (Provider, error) {
	// Collect non-nil providers.
	enabled := make([]Provider, 0, len(r.providers))
	for _, p := range r.providers {
		if p != nil {
			enabled = append(enabled, p)
		}
	}

	switch {
	case len(enabled) == 0:
		return nil, ErrNoProvider
	case len(enabled) == 1:
		if activeName == "" || activeName == enabled[0].Name() {
			return enabled[0], nil
		}
		return nil, fmt.Errorf("%w: specified %q, registered %q",
			ErrProviderNotFound, activeName, enabled[0].Name())
	default:
		// Multiple providers.
		if activeName == "" {
			return nil, ErrMultipleProviders
		}
		for _, p := range enabled {
			if p.Name() == activeName {
				return p, nil
			}
		}
		return nil, fmt.Errorf("%w: specified %q, registered: %v",
			ErrProviderNotFound, activeName, providerNames(enabled))
	}
}

func providerNames(providers []Provider) []string {
	names := make([]string, len(providers))
	for i, p := range providers {
		names[i] = p.Name()
	}
	return names
}

func (r *Registry) initConsumer(ctx context.Context, c Consumer) error {
	if err := ValidateConsumerTables(c.Name(), c.Tables()); err != nil {
		return fmt.Errorf("validate consumer %q tables: %w", c.Name(), err)
	}

	for _, t := range c.Tables() {
		realTable := c.Name() + "_" + t.Name
		ddl, err := RenderDDL(t.Schema, realTable, "")
		if err != nil {
			return fmt.Errorf("consumer %q table %q: %w", c.Name(), t.Name, err)
		}
		if _, err := r.active.DB().ExecContext(ctx, ddl); err != nil {
			return fmt.Errorf("create table %q for consumer %q: %w", realTable, c.Name(), err)
		}

		for _, idx := range t.Indexes {
			realIndex := fmt.Sprintf("idx_%s_%s_%s", c.Name(), t.Name, idx.Name)
			ddl, err := RenderDDL(idx.SQL, realTable, realIndex)
			if err != nil {
				return fmt.Errorf("consumer %q index %q: %w", c.Name(), idx.Name, err)
			}
			if _, err := r.active.DB().ExecContext(ctx, ddl); err != nil {
				return fmt.Errorf("create index %q for consumer %q: %w", realIndex, c.Name(), err)
			}
		}
	}

	store := newBoundStore(c.Name(), r.active.Dialect(), r.active.DB(), c.Tables())
	r.stores[c.Name()] = store
	if err := c.BindStore(store); err != nil {
		delete(r.stores, c.Name())
		return fmt.Errorf("bind store for consumer %q: %w", c.Name(), err)
	}
	return nil
}

func (r *Registry) disableAllConsumers(reason error) {
	r.logError("persistence consumer disabled: no provider registered", "error", reason,
		"consumer_count", len(r.consumers))
	for _, c := range r.consumers {
		c.DisablePersistence(reason)
	}
}

// Shutdown closes the active provider connection.
func (r *Registry) Shutdown() error {
	if r.active != nil {
		return r.active.Close()
	}
	return nil
}

// ActiveProvider returns the selected provider, or nil if none is active.
func (r *Registry) ActiveProvider() Provider {
	return r.active
}

// StoreForConsumer returns the Store bound to the given consumer name.
// Returns nil, false if no store is bound (e.g. persistence disabled).
func (r *Registry) StoreForConsumer(name string) (Store, bool) {
	s, ok := r.stores[name]
	return s, ok
}

// logError is a nil-safe logger wrapper.
func (r *Registry) logError(msg string, args ...any) {
	if r.logger != nil {
		r.logger.Error(msg, args...)
	}
}
