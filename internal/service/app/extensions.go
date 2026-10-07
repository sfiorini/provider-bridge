package app

import (
	"database/sql"
	"log/slog"

	"providerbridge/internal/config"
	codextoolproxy "providerbridge/internal/extension/codex_tool_proxy"
	dbd1 "providerbridge/internal/extension/db/d1"
	dbsqlite "providerbridge/internal/extension/db/sqlite"
	deepseekv4 "providerbridge/internal/extension/deepseek_v4"
	kimiworkaround "providerbridge/internal/extension/kimi_workaround"
	mbtrics "providerbridge/internal/extension/metrics"
	"providerbridge/internal/extension/plugin"
	"providerbridge/internal/extension/visual"
)

// ExtensionOptions controls optional initialization of built-in plugins.
type ExtensionOptions struct {
	// D1DB is an optional *sql.DB for Cloudflare D1. Only set in Worker env.
	D1DB *sql.DB
}

type BuiltinExtensionCatalog struct {
	Opts ExtensionOptions
}

func BuiltinExtensions() BuiltinExtensionCatalog {
	return BuiltinExtensionCatalog{}
}

func (cat BuiltinExtensionCatalog) ConfigSpecs() []config.ExtensionConfigSpec {
	var specs []config.ExtensionConfigSpec
	specs = append(specs, deepseekv4.ConfigSpecs()...)
	specs = append(specs, kimiworkaround.ConfigSpecs()...)
	specs = append(specs, visual.ConfigSpecs()...)
	specs = append(specs, dbsqlite.ConfigSpecs()...)
	specs = append(specs, dbd1.ConfigSpecs()...)
	specs = append(specs, mbtrics.ConfigSpecs()...)
	specs = append(specs, codextoolproxy.ConfigSpecs()...)
	return specs
}

func (cat BuiltinExtensionCatalog) NewRegistry(logger *slog.Logger, cfg config.Config) *plugin.Registry {
	registry := plugin.NewRegistry(logger)
	registry.Register(kimiworkaround.NewPlugin(func(model string) bool {
		return cfg.ExtensionEnabled("kimi_workaround", model)
	}))
	registry.Register(deepseekv4.NewPlugin(func(model string) bool {
		return cfg.ExtensionEnabled("deepseek_v4", model)
	}))
	registry.Register(visual.NewPlugin())
	registry.Register(dbsqlite.NewPlugin())

	// D1 provider: inject DB if provided, otherwise register as regular plugin.
	d1 := dbd1.NewPlugin()
	if cat.Opts.D1DB != nil {
		d1.InjectDB(cat.Opts.D1DB)
	}
	registry.Register(d1)

	registry.Register(mbtrics.NewPlugin())
	registry.Register(codextoolproxy.NewPlugin())
	return registry
}
