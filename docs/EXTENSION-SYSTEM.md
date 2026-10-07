# Extension System

Provider Bridge extensions are a capability-interface plugin architecture. A
plugin implements the base `Plugin` interface plus zero or more capability
interfaces to extend the bridge.

## Core interfaces

### Plugin (base interface)

Every plugin implements `plugin.Plugin`:

```go
// internal/extension/plugin/plugin.go
type Plugin interface {
    Name() string                    // unique identifier (e.g. "deepseek_v4")
    Init(ctx PluginContext) error    // initialize; receives config
    Shutdown() error                 // release resources
    EnabledForModel(modelAlias string) bool  // active for this model?
}
```

```go
type PluginContext struct {
    Config        any                  // decoded typed config (or nil)
    AppConfig     config.Config         // read-only init-time snapshot
    CurrentConfig func() config.Config  // latest config at request time
    Logger        *slog.Logger          // logger prefixed with the plugin name
}
```

The built-in `BasePlugin` provides no-op defaults for all methods, so a plugin
only overrides what it needs.

### RequestContext and StreamContext

Capability methods take a `*RequestContext` or `*StreamContext`
(`internal/extension/plugin/context.go`):

```go
type RequestContext struct {
    ModelAlias  string               // model alias (e.g. "provider-bridge")
    SessionData map[string]any       // per-session state, keyed by plugin name
    Reasoning   map[string]any       // OpenAI reasoning config
    WebSearch   WebSearchInfo        // resolved web-search settings
}

type StreamContext struct {
    RequestContext
    StreamState any  // the plugin's per-stream state
}

func (ctx *RequestContext) SessionState(pluginName string) any {
    // returns that plugin's session state
}
```

Session data is isolated by `session.Session` — sessions (identified by
`session_id`, `X-Codex-Window-Id`, or `X-Claude-Code-Session-Id`) each have
their own `ExtensionData` map.

### ConfigSpecProvider

A plugin declares its own configuration via `ConfigSpecProvider`, scoped across
global/provider/model/route:

```go
type ConfigSpecProvider interface {
    ConfigSpecs() []config.ExtensionConfigSpec
}
```

## Capability interfaces

Plugins implement any subset of the following. `plugin.Registry` detects them
by type assertion at registration time and chains the implementations inside
`CorePluginHooks()`.

#### Request pipeline

| Interface | Method | When it runs |
|-----------|--------|--------------|
| `InputPreprocessor` | `PreprocessInput(ctx, raw) json.RawMessage` | Before the input JSON is deserialized |
| `MessageRewriter` | `RewriteMessages(ctx, messages) []CoreMessage` | After the message list is converted |
| `RequestMutator` | `MutateRequest(ctx, req)` | After the CoreRequest is built, before the provider adapter |
| `ToolInjector` | `InjectTools(ctx) []CoreTool` | During tool conversion; returns extra tools |

#### Provider pipeline

| Interface | Method | When it runs |
|-----------|--------|--------------|
| `ProviderWrapper` | `WrapProvider(ctx, provider) any` | Wraps the upstream provider client |

#### Response pipeline

| Interface | Method | When it runs |
|-----------|--------|--------------|
| `ContentFilter` | `FilterContent(ctx, block) bool` | Per response content block; `true` skips it |
| `ResponsePostProcessor` | `PostProcessResponse(ctx, resp)` | After the final OpenAI Response is built |
| `ContentRememberer` | `RememberContent(ctx, content)` | When the full response content is available |

#### Streaming pipeline

| Interface | Method | When it runs |
|-----------|--------|--------------|
| `StreamInterceptor` | `NewStreamState() any` | Creates per-request stream state |
| | `OnStreamEvent(ctx, event) (consumed, emit)` | Per stream event; `consumed=true` skips normal handling |
| | `OnStreamComplete(ctx, outputText)` | Stream finished |

```go
// internal/extension/plugin/capabilities.go
type StreamEvent struct {
    Type  string  // "block_start", "block_delta", "block_stop"
    Index int
    Block *format.CoreContentBlock  // for block_start
    Delta anthropic.StreamDelta     // for block_delta
}
```

#### History reconstruction

| Interface | Method | When it runs |
|-----------|--------|--------------|
| `ThinkingPrepender` | `PrependThinkingForToolUse(messages, toolCallID, summary, state) []CoreMessage` | Before a tool call |
| | `PrependThinkingForAssistant(blocks, summary, state) []CoreContentBlock` | Before an assistant message |
| `ReasoningExtractor` | `ExtractThinkingBlock(ctx, summary) (CoreContentBlock, bool)` | Restores a thinking block from a reasoning summary |

#### Error handling

| Interface | Method | When it runs |
|-----------|--------|--------------|
| `ErrorTransformer` | `TransformError(ctx, msg) string` | Rewrites upstream error messages |

#### Session state

| Interface | Method | When it runs |
|-----------|--------|--------------|
| `SessionStateProvider` | `NewSessionState() any` | New session created |

#### Logging

| Interface | Method | When it runs |
|-----------|--------|--------------|
| `LogConsumer` | `ConsumeLog(ctx, entries) []LogEntry` | Each slog entry through the consume pipeline; may intercept, modify or suppress |

#### Request completion and HTTP routing

| Interface | Method | When it runs |
|-----------|--------|--------------|
| `RequestCompletionHook` | `OnRequestCompleted(ctx, result)` | After every request, with model, tokens, cost, status and duration |
| `RouteRegistrar` | `RegisterRoutes(register)` | Server init; registers extra HTTP handlers |

#### Persistence

| Interface | Method | When it runs |
|-----------|--------|--------------|
| `DBProvider` | `DBProvider() db.Provider` | Declares a database backend (SQLite, D1) |
| `DBConsumer` | `DBConsumer() db.Consumer` | Declares a consumer that needs the database (e.g. metrics) |

#### Core-format adapter interfaces (Core path)

These adapters integrate with the Core path (`internal/format`):

| Interface | Method | When it runs |
|-----------|--------|--------------|
| `CoreRequestMutator` | `MutateCoreRequest(ctx, req)` | After the CoreRequest is built (standard `context.Context`) |
| `CoreContentFilter` | `FilterCoreContent(ctx, block) bool` | Filters Core content blocks |
| `CoreContentRememberer` | `RememberCoreContent(ctx, content)` | Remembers Core content blocks |

## Registry

`plugin.Registry` owns every registered plugin, bucketed by capability. It
also carries a `PatchProxyDecider` hook through `CorePluginHooks`.

```go
// internal/extension/plugin/registry.go
type Registry struct {
    plugins                []Plugin
    inputPreprocessors     []InputPreprocessor
    requestMutators        []RequestMutator
    toolInjectors          []ToolInjector
    messageRewriters       []MessageRewriter
    providerWrappers       []ProviderWrapper
    contentFilters         []ContentFilter
    responsePostProcs      []ResponsePostProcessor
    contentRememberers     []ContentRememberer
    streamInterceptors     []StreamInterceptor
    errorTransformers      []ErrorTransformer
    sessionProviders       []SessionStateProvider
    logConsumers           []LogConsumer
    dbProviders            []DBProvider
    dbConsumers            []DBConsumer
    requestCompletionHooks []RequestCompletionHook
    usageSources           []UsageSource
    routeRegistrars        []RouteRegistrar
    configSpecs            []config.ExtensionConfigSpec
    logger                 *slog.Logger
    currentConfig          func() config.Config
}
```

### Registration flow

```go
// 1. Create the registry
registry := plugin.NewRegistry(logger.L())

// 2. Register plugins (capabilities auto-detected)
registry.Register(deepseekv4.NewPlugin())
registry.Register(visual.NewPlugin())
registry.Register(dbsqlite.NewPlugin())
registry.Register(metrics.NewPlugin())

// 3. Initialize (passes AppConfig and decoded typed extension config)
if err := registry.InitAll(&cfg); err != nil {
    // cfg.ExtensionConfig("deepseek_v4", "") → *deepseekv4.Config
}

// 4. Build CorePluginHooks (chains all plugin capabilities)
hooks := registry.CorePluginHooks()

// 5. Clean up on shutdown
defer registry.ShutdownAll()
```

`Registry.CorePluginHooks()` (in `registry.go`) walks the registered plugins and
chains those implementing the Core capability interfaces into the matching
`format.CorePluginHooks` fields, gating each on the plugin's
`EnabledForModel` where a model alias is available.

## Integration with adapters

Plugins integrate with the adapter layer through `format.CorePluginHooks`
(defined in `internal/format/adapter.go`), a function struct built by
`Registry.CorePluginHooks()`:

```go
type CorePluginHooks struct {
    PreprocessInput            func(ctx context.Context, model string, raw json.RawMessage) json.RawMessage
    RewriteMessages            func(ctx context.Context, req *CoreRequest)
    InjectTools                func(ctx context.Context) []CoreTool
    MutateCoreRequest          func(ctx context.Context, req *CoreRequest)
    PostProcessCoreResponse    func(ctx context.Context, resp *CoreResponse)
    TransformError             func(ctx context.Context, model string, msg string) string
    OnStreamEvent              func(ctx context.Context, event CoreStreamEvent) (skip bool)
    OnStreamComplete           func(ctx context.Context, model string, outputText string)
    FilterContent              func(ctx context.Context, block *CoreContentBlock) (skip bool)
    RememberContent            func(ctx context.Context, content []CoreContentBlock)
    NewStreamState             func(ctx context.Context, model string) any
    PrependThinkingToAssistant func(ctx context.Context, req *CoreRequest)
    DisablePatchProxy          func(model string) bool
}

func (hooks CorePluginHooks) WithDefaults() CorePluginHooks {
    // replaces every nil function with a no-op for safe calling
}
```

Adapters call the hooks during conversion:

```go
// Upstream provider adapter:
a.hooks.MutateCoreRequest(ctx, req)     // modify the CoreRequest
a.hooks.RememberContent(ctx, content)   // remember response content

// Inbound client adapter:
a.hooks.PreprocessInput(ctx, model, raw)     // preprocess input
a.hooks.PostProcessCoreResponse(ctx, resp)   // post-process response
```

Both inbound paths use the same hooks: the original Responses dispatch
(`handleWithAdapters` / `handleAdapterStream`) and the Core executor
(`executeCoreUpstream`) used by the Anthropic Messages and Chat Completions
inbounds. The import direction is unchanged either way — `internal/protocol/*`
depends on `format`, never on `service` or `extension`.

The server layer also consumes plugin capabilities directly:

- `LogConsumer` — wired through `logger.SetConsumeFunc()` into log buffering.
- `DBProvider` / `DBConsumer` — `db.Registry` initializes the database and binds
  consumers.
- `RequestCompletionHook` — fired by `server.onRequestCompleted()` after each
  request.
- `RouteRegistrar` — mounted onto the `http.ServeMux` by
  `server.registerPluginRoutes()`.

`websearchinjected` implements plugin interfaces, but in the current runtime
the bridge/server calls its tool and provider-wrapper functions directly based
on the model's resolved web-search mode; it is **not** registered by
`BuiltinExtensions()`.

## Configuration

Extension parameters live under `extensions`; the plugin owns its `config:`
block and enablement lives in the matching scope's `enabled`:

```yaml
extensions:
  deepseek_v4:
    config:
      reinforce_instructions: true
      reinforce_prompt: "[System Reminder]: ...\n[User]:"
```

A plugin declares its config shape via `ConfigSpecProvider`:

```go
func (p *DSPlugin) ConfigSpecs() []config.ExtensionConfigSpec {
    return []config.ExtensionConfigSpec{{
        Name: "deepseek_v4",
        Scopes: []config.ExtensionScope{
            config.ExtensionScopeGlobal,
            config.ExtensionScopeProvider,
            config.ExtensionScopeModel,
            config.ExtensionScopeRoute,
        },
        Factory: func() any { return &Config{} },
    }}
}

func (p *DSPlugin) Init(ctx plugin.PluginContext) error {
    p.cfg = plugin.Config[Config](ctx)  // decoded from PluginContext
    p.appCfg = ctx.AppConfig
    return nil
}

func (p *DSPlugin) EnabledForModel(model string) bool {
    return p.appCfg.ExtensionEnabled("deepseek_v4", model)
}
```

## Implementation demos

### Minimal plugin

```go
package demo

import (
    "providerbridge/internal/extension/plugin"
)

const PluginName = "demo"

type DemoConfig struct {
    Prefix string `json:"prefix,omitempty" yaml:"prefix"`
}

type DemoPlugin struct {
    plugin.BasePlugin
    prefix string
}

func NewPlugin() *DemoPlugin {
    return &DemoPlugin{}
}

func (p *DemoPlugin) Name() string { return PluginName }

func (p *DemoPlugin) Init(ctx plugin.PluginContext) error {
    cfg := plugin.Config[DemoConfig](ctx)
    if cfg != nil {
        p.prefix = cfg.Prefix
    }
    ctx.Logger.Info("demo plugin initialized", "prefix", p.prefix)
    return nil
}

func (p *DemoPlugin) EnabledForModel(model string) bool {
    return true  // enabled for all models
}
```

### Plugin with capabilities

```go
package demo

import (
    "providerbridge/internal/extension/plugin"
    "providerbridge/internal/format"
)

// Injects an extra system instruction and tool.
type SystemInjectionPlugin struct {
    plugin.BasePlugin
    systemMessage string
}

func (p *SystemInjectionPlugin) Name() string { return "system_inject" }

// --- RequestMutator (modifies the CoreRequest) ---
func (p *SystemInjectionPlugin) MutateRequest(ctx *plugin.RequestContext, req *format.CoreRequest) {
    req.System = append(req.System, format.CoreContentBlock{
        Type: "text",
        Text: p.systemMessage,
    })
}

// --- ToolInjector (injects an extra tool) ---
func (p *SystemInjectionPlugin) InjectTools(ctx *plugin.RequestContext) []format.CoreTool {
    return []format.CoreTool{{
        Name:        "get_current_time",
        Description: "Get the current system time",
        InputSchema: map[string]any{"type": "object"},
    }}
}

// Compile-time interface assertions
var (
    _ plugin.Plugin         = (*SystemInjectionPlugin)(nil)
    _ plugin.ToolInjector   = (*SystemInjectionPlugin)(nil)
    _ plugin.RequestMutator = (*SystemInjectionPlugin)(nil)
)
```

### Registering the demo

```go
// In service/app: after NewRegistry(...)
registry.Register(demo.NewPlugin())
if err := registry.InitAll(&cfg); err != nil {
    return fmt.Errorf("init plugins: %w", err)
}
defer registry.ShutdownAll()
```

`registry.CorePluginHooks()` then builds the `format.CorePluginHooks` the
adapters consume.
