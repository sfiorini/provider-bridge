# Extensions

Catalogue of the extensions shipped with Provider Bridge: each entry lists its
location, files, implemented capabilities, flow and configuration. For the
plugin architecture itself see [EXTENSION-SYSTEM.md](EXTENSION-SYSTEM.md).

## deepseek_v4 (DeepSeek V4 support)

Makes DeepSeek V4 models work correctly over Anthropic-compatible endpoints.
DeepSeek V4 implements a subset of the Anthropic Messages API with a few
differences that need handling.

**Location:** `internal/extension/deepseek_v4/`

**Files:**

| File | Purpose |
|------|---------|
| `plugin.go` | Plugin implementation; registers all capabilities |
| `deepseek_v4.go` | Core conversion functions (`reasoning_content` handling) |
| `state.go` | Thinking cache state management |

**Capabilities:**

```go
// Compile-time assertions (plugin.go)
var (
    _ plugin.Plugin               = (*DSPlugin)(nil)
    _ plugin.InputPreprocessor    = (*DSPlugin)(nil)
    _ plugin.RequestMutator       = (*DSPlugin)(nil)
    _ plugin.MessageRewriter      = (*DSPlugin)(nil)
    _ plugin.ContentFilter        = (*DSPlugin)(nil)
    _ plugin.ContentRememberer    = (*DSPlugin)(nil)
    _ plugin.StreamInterceptor    = (*DSPlugin)(nil)
    _ plugin.ErrorTransformer     = (*DSPlugin)(nil)
    _ plugin.SessionStateProvider = (*DSPlugin)(nil)
    _ plugin.ThinkingPrepender    = (*DSPlugin)(nil)
    _ plugin.ReasoningExtractor   = (*DSPlugin)(nil)
)
```

### InputPreprocessor

`PreprocessInput()` removes the `reasoning_content` field from input messages.
DeepSeek returns HTTP 400 if `reasoning_content` appears in input, because the
field is output-only.

### RequestMutator

`MutateRequest()` calls `ToAnthropicRequest()` to adapt the request for
DeepSeek:

- Clears `Temperature` and `TopP` (DeepSeek may reject them).
- Maps OpenAI `reasoning.effort` to Anthropic `output_config.effort`
  (`high` → `high`, `xhigh`/`max` → `max`).

### MessageRewriter

`RewriteMessages()` optionally prepends a reinforcement prompt to user messages
to remind the model to follow the system prompt and AGENTS.md.

### ContentFilter + ContentRememberer + ThinkingPrepender + ReasoningExtractor

This group solves **thinking-history reconstruction**, the core of the
extension.

**The problem.** On the next turn, DeepSeek V4 requires the previous turn's
`thinking` block (Anthropic `type: "thinking"` content block) to be present in
the input history, or it returns an error.

**The Codex constraint.** Codex's Conversations API keeps only the `reasoning`
summary (`OutputItem.Type: "reasoning"`), not the full thinking text.

**The solution (four steps):**

```
1. On response (ContentFilter) → intercept the upstream thinking block, extract it as a reasoning summary
2. On remember (ContentRememberer) → cache the thinking block in SessionData by tool_call_id / text_hash
3. On replay (ThinkingPrepender + ReasoningExtractor) → in the next turn's request:
   a. first restore the original thinking block from the reasoning summary (Encode/DecodeThinkingSummary)
   b. fall back to the thinking cached in SessionData by tool_call_id
   c. finally insert an empty thinking block as a last resort
4. Keep learning (StreamInterceptor) → capture and cache thinking in streaming mode too
```

### StreamInterceptor

In streaming mode, intercepts `thinking_delta` / `reasoning_content_delta`
events, accumulates the full thinking text, and caches it in session state when
the stream ends.

### ErrorTransformer

Rewrites DeepSeek-specific errors: messages about "thinking mode" become friendlier,
human-readable text.

### SessionStateProvider

Creates a `*State` for cross-request thinking caching. `State` maintains two
LRU maps:

- `records`: thinking blocks indexed by `tool_use_id` (up to 1024 entries)
- `textRecords`: thinking blocks indexed by the assistant text SHA256 (up to
  1024 entries)

### Enabling

```yaml
models:
  deepseek-v4-pro:
    extensions:
      deepseek_v4:
        enabled: true
```

Routes inherit the model's `deepseek_v4` extension settings; no separate
route-level switch is required. `EnabledForModel` checks
`Config.ExtensionEnabled("deepseek_v4", model)`.

---

## web_search_injected (injected web search)

When an upstream does not support Anthropic's native `web_search_20250305`
server tool, Provider Bridge can use "injected" mode: it injects
`tavily_search` and `firecrawl_fetch` as function tools and executes the
searches server-side.

**Location:** `internal/extension/websearchinjected/`

In the current runtime this is **not** an independent plugin registered by
`BuiltinExtensions()`; the bridge/server calls the module's `InjectTools()` and
`WrapProvider()` directly based on the model's resolved web-search mode.
`plugin.go` keeps the plugin interface for module boundaries and tests.

**Files:**

| File | Purpose |
|------|---------|
| `plugin.go` | Plugin implementation |
| `websearchinjected.go` | Core tool/wrapper functions |

**Capabilities:**

```go
var (
    _ plugin.Plugin          = (*WSInjectedPlugin)(nil)
    _ plugin.ToolInjector    = (*WSInjectedPlugin)(nil)
    _ plugin.ProviderWrapper = (*WSInjectedPlugin)(nil)
)
```

### Flow

```
1. The consumer request includes a web_search_preview tool
2. The bridge resolves the model's web-search mode → "injected"
3. The bridge injects tools via websearch.Tools() / websearchinjected.InjectTools():
   - tavily_search (function tool)
   - firecrawl_fetch (function tool, when a Firecrawl key is configured)
4. The server executes the search loop, feeding results back as tool_result
   until the model stops requesting searches or hits SearchMaxRounds
```

On the **Core path** (Anthropic Messages / Chat Completions inbounds, including
the visual orchestrator) the loop is `executeCoreSearchLoop`; on the
wire-level chat/openai path it is `executeChatSearchLoop` /
`chatSearchBufferedStream`. Search runs entirely inside the bridge — no
`tool_use` blocks leak to the consumer.

### Orchestrator

`websearch.NewInjectedOrchestrator()` builds a search orchestrator that wraps an
Anthropic client and exposes the same `CreateMessage` / `StreamMessage`
surface. It loops over search tool calls until the model stops requesting
searches or reaches `SearchMaxRounds`.

### Configuration

Provider level:

```yaml
providers:
  my-provider:
    base_url: "https://..."
    api_key: "..."
    web_search:
      support: "injected"
      tavily_api_key: "tvly-..."
      firecrawl_api_key: "fc-..."
      search_max_rounds: 5
```

Global:

```yaml
web_search:
  support: "injected"
  tavily_api_key: "tvly-..."
  firecrawl_api_key: "fc-..."
  search_max_rounds: 5
```

Model-level override:

```yaml
models:
  my-model:
    web_search:
      support: "enabled"   # overrides the provider-level "injected"
```

See [WEB-SEARCH.md](WEB-SEARCH.md) for the full precedence rules and the
startup-only resolution caveat.

## kimi_workaround (Kimi tool-call round limiter)

Kimi models sometimes fall into unbounded information-gathering loops with
tools. `kimi_workaround` injects progress and limit hints near the maximum
tool-call round, nudging the model to summarize and stop calling tools.

**Location:** `internal/extension/kimi_workaround/`

**Files:**

| File | Purpose |
|------|---------|
| `plugin.go` | Plugin implementation; registers all capabilities |

**Capabilities:**

- `InputPreprocessor` — preprocess input messages
- `ContentFilter` — filter response content
- `ContentRememberer` — remember content blocks for round tracking
- `StreamInterceptor` — stream event interception + round tracking
- `SessionStateProvider` — cross-request round state

### Enabling

```yaml
models:
  my-kimi-model:
    extensions:
      kimi_workaround:
        enabled: true
```

Global config:

```yaml
extensions:
  kimi_workaround:
    config:
      max_tool_rounds: 50
      convergence_margin: 0.8
```

---

## codex (Codex compatibility toolkit)

Not a classic `Plugin`, but `internal/extension/codex/` is an important part of
the extension surface.

**Location:** `internal/extension/codex/`

**Files:**

| File | Purpose |
|------|---------|
| `catalog.go` | Model catalog DTO generation, Codex `config.toml` generation |
| `default_instructions.go` | Default model-instruction templates (embeds `default_instructions.txt`) |

### Responsibilities

1. **Model catalog** — generate Codex CLI's `models_catalog.json` and
   `config.toml` from the bridge config.
2. **Default instruction injection** — supply Codex-adapted default system
   instructions.

### CLI integration

```bash
providerbridge -config config.yml -print-codex-config my-model
```

---

## codex_tool_proxy (apply_patch proxy)

Controls whether Codex's `apply_patch` custom tool is expanded into five
structured proxy tools (`add_file`, `delete_file`, `update_file`,
`replace_file`, `batch`) before being sent upstream.

**Location:** `internal/extension/codex_tool_proxy/`

**Files:**

| File | Purpose |
|------|---------|
| `plugin.go` | Plugin implementation + `PatchProxyDecider` |

**Behavior:**

- **Off by default** (`DefaultEnabled: false`): `apply_patch` is passed through
  upstream as raw grammar.
- **Enabled:** expanded into five independent structured tools that the
  upstream model calls via JSON schema.

**Capabilities:**

```go
var (
    _ plugin.Plugin             = (*ProxyPlugin)(nil)
    _ plugin.ConfigSpecProvider = (*ProxyPlugin)(nil)
    _ plugin.PatchProxyDecider  = (*ProxyPlugin)(nil)
)
```

**Enabling:**

```yaml
extensions:
  codex_tool_proxy:
    enabled: true
```

Route / model / provider-level overrides are supported.

## visual (visual orchestration)

When the primary model is not multimodal, Provider Bridge can delegate image
analysis to a dedicated vision provider. The `visual` extension acts as a
`ToolInjector`, injecting `visual_brief` and `visual_qa` tools into the primary
model's conversation. On the server, `wrapWithVisual()` wraps the upstream
provider as a `CoreProvider` and intercepts visual tool calls at the Core
layer, delegating them to the configured vision provider.

**Location:** `internal/extension/visual/`

**Files:**

| File | Purpose |
|------|---------|
| `plugin.go` | Plugin implementation; injects `visual_brief` / `visual_qa`; exposes `ConfigForModel` |
| `core_orchestrator.go` | Core-layer orchestrator (current path) |
| `orchestrator.go` | Legacy Anthropic provider-wrapper orchestrator |
| `client.go` | `CoreProvider` interface + `BridgeClient` implementation |
| `chat_strip.go` | Chat-path image stripping/placeholder handling |
| `tools.go` | Tool definitions and schema generation |
| `types.go` | Type definitions |
| `legacy.go` | Legacy helpers |

**Capabilities:**

```go
var (
    _ plugin.Plugin       = (*Plugin)(nil)
    _ plugin.ToolInjector = (*Plugin)(nil)
)
```

### Flow

1. The request reaches the server; the visual orchestrator wraps the upstream
   provider.
2. The orchestrator scans request messages for image blocks and replaces them
   with text placeholders (`Image #1`, `Image #2`, …).
3. The primary model processes the request and may call `visual_brief` /
   `visual_qa`.
4. The orchestrator intercepts the call:
   - extracts `image_refs` / `image_urls` from the tool arguments
   - matches them against the saved `availableImages`
   - sends them to the vision provider via `VisionClient.Analyze()`
5. The analysis is returned to the primary model as a `tool_result`.
6. The primary model continues, optionally asking follow-ups with
   `visual_qa`.

The `needsAssist` gate (`hasImage && !supportsImg`) decides when orchestration
runs: it only fires when the request actually contains an image and the chosen
model does not advertise image input.

### Vision provider

Analysis runs through the `VisionClient` interface. The built-in `BridgeClient`
uses a separate Anthropic-compatible provider, so any multimodal provider
(Kimi, GPT-4o, …) can serve as the vision backend.

```go
type VisionClient interface {
    Analyze(context.Context, AnalysisRequest) (string, error)
}
```

### Configuration

```yaml
extensions:
  visual:
    config:
      provider: "visual-backend"
      model: "kimi-vision-model"
      max_tokens: 4096

models:
  my-model:
    extensions:
      visual:
        enabled: true
```

The vision model's `input_modalities` must include `image`; it is eligible to
receive images only when it does.

### Interaction with providers

The visual orchestrator works at the Core layer — `wrapWithVisual()` (in
`internal/service/server/adapter_dispatch.go`) wraps the upstream provider as a
`CoreProvider` and intercepts Core-format requests/responses. When the primary
model cannot handle images and calls a visual tool, the orchestrator forwards
the image to the configured vision provider and returns the analysis.

---

## db_sqlite (SQLite persistence provider)

Local-process database backend. Persistence is a first-class, stable capability
in the current tree, though the extension surface itself is still evolving.

**Location:** `internal/extension/db/sqlite/`

**Capabilities:**

```go
var (
    _ plugin.Plugin             = (*Plugin)(nil)
    _ plugin.ConfigSpecProvider = (*Plugin)(nil)
    _ plugin.DBProvider         = (*Plugin)(nil)
)
```

```yaml
extensions:
  db_sqlite:
    enabled: true
    config:
      path: ./data/provider-bridge.db
      wal: true
      busy_timeout_ms: 5000
      max_open_conns: 1
```

When `path` is empty or `enabled: false`, no database is provided. WAL is
enabled by default, the default busy timeout is 5000 ms, and the default max
open connections is 1.

---

## db_d1 (Cloudflare D1 persistence provider)

Database backend for the Cloudflare Worker environment. It depends on the
Worker entry point injecting the database.

**Location:** `internal/extension/db/d1/`

The D1 provider does not import the Cloudflare Workers SDK directly; the Worker
entry point calls `InjectDB()` with a `*sql.DB` before init. In a plain local
process the provider stays unavailable even if a binding is configured.

```yaml
extensions:
  db_d1:
    enabled: true
    config:
      binding: PROVIDER_BRIDGE_DB
```

---

## metrics (request metrics)

Records each request's model, actual upstream model, tokens, cost, status,
error and duration, and exposes a query endpoint when a database is available.

**Location:** `internal/extension/metrics/`

**Capabilities:**

```go
var (
    _ plugin.Plugin                = (*Plugin)(nil)
    _ plugin.ConfigSpecProvider    = (*Plugin)(nil)
    _ plugin.RequestCompletionHook = (*Plugin)(nil)
    _ plugin.RouteRegistrar        = (*Plugin)(nil)
    _ plugin.DBConsumer            = (*Plugin)(nil)
)
```

```yaml
extensions:
  metrics:
    enabled: true
    config:
      default_limit: 100
      max_limit: 1000
```

Once metrics binds to the database store it registers `GET /v1/admin/metrics`,
supporting `limit`, `offset`, `model`, `status`, `since`, `until` and
`order=asc` query parameters.
