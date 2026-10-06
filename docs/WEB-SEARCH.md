# Web Search

Provider Bridge can execute web search **server-side** on behalf of a model.
When a consumer requests web search, the bridge replaces the client's
`web_search` tool with its own tools (`tavily_search`, `firecrawl_fetch`),
executes them inside the bridge (Tavily for search, Firecrawl for page fetch),
feeds the results back to the model, and returns a normal answer. The consumer
never sees the tool calls or results.

This is the "injected" mode. It works for upstream protocols that have no
native web search (`openai-chat`, `google-genai`) and can also be used for
Anthropic providers instead of their native `web_search_20250305` server tool.

> **Startup-only resolution.** From the project's architecture memory:
> "Resolution is **startup-only** — any web_search config change requires a
> container restart." The support mode and per-provider/model overrides are
> resolved by `resolvePerProviderWebSearch` during application bootstrap
> (`internal/service/app/app.go`); the hot-reload path
> (`runtime.Reload` → `NewProviderManager`) rebuilds the provider manager
> without re-running that resolution. **Restart the container after changing
> any web-search setting.**

## Configuration scopes

Web-search settings resolve from most specific to least specific. The support
mode follows:

```
route  →  model catalog entry  →  provider  →  global
```

(`config.WebSearchForModel`, `internal/config/config.go`.)

| Scope | Field location | Example |
|-------|----------------|---------|
| Global | `web_search:` (top level) | `web_search.support: injected` |
| Provider | `providers.<key>.web_search:` | `providers.mistral.web_search.support: disabled` |
| Model | `providers.<key>.models.<model>.web_search:` | per-model override |
| Route | `routes.<alias>.web_search:` | per-alias override |

API keys and round limits resolve the same way:

- `tavily_api_key`, `firecrawl_api_key`, `search_max_rounds` may be set
  globally, per provider, or per model. Model-level wins, then provider-level,
  then global (`resolvedSearchConfig`, `internal/service/server/adapter_dispatch.go`).
- The global default for `search_max_rounds` is `5`.
- If no Tavily key is available, injection is skipped (`openai-chat` /
  `google-genai`) or disabled at startup.

### Support modes

| Mode | Behavior |
|------|----------|
| `auto` (default) | Probe the provider's native web search (Anthropic). If the probe fails and a Tavily key exists, fall back to `injected`. |
| `enabled` | Force the provider's native web search (Anthropic / Responses upstream). |
| `disabled` | No web search. |
| `injected` | Inject `tavily_search` / `firecrawl_fetch` and execute server-side. |

Per-protocol interpretation (`resolvePerProviderWebSearch`):

- **`anthropic`** — `disabled` → disabled; `enabled` → native web search;
  `injected` → injection; `auto` → probe, then fall back to injection when a
  Tavily key exists.
- **`openai-response`** — `disabled` or `injected` → disabled (Responses
  upstreams do not accept injected function tools); otherwise native enabled.
- **`openai-chat` / `google-genai`** — no native web search: `disabled` →
  disabled; `injected` → injection; anything else → injection when a global
  Tavily key exists, otherwise disabled.

## Execution loops

Once the support mode resolves, the request path for that inbound determines
which loop runs:

| Loop | File | Path |
|------|------|------|
| `executeChatSearchLoop` / `chatSearchBufferedStream` | `internal/service/server/websearch_inject.go` | Wire-level, `openai-chat` upstreams |
| `executeCoreSearchLoop` | `internal/service/server/websearch_inject.go` | Core-level, used by the visual orchestrator path |
| `executeGoogleSearchLoop` | `internal/service/server/websearch_inject.go` | `google-genai` upstreams |
| `websearchinjected.WrapProvider` (orchestrator) | `internal/extension/websearchinjected` | Anthropic provider clients in the Responses path and `executeCoreUpstream` |

`executeCoreSearchLoop` exists because the visual-orchestrator path bypasses
the protocol-specific search loops: without it, `tavily_search` /
`firecrawl_fetch` calls would be forwarded to the consumer, which cannot
execute them. It appends the assistant message (without reasoning blocks) plus
`tool` role results and re-calls the upstream, up to `maxRounds`.

Mixed responses (search calls plus non-search tool calls) are handled by
executing the search calls as a side effect and returning the response so the
consumer handles the remaining tool calls on the next round-trip.

## Tool-name substitution

Injection replaces the client's `web_search` / `web_search_preview` tool with
bridge-owned function tools:

- `tavily_search` — parameters `query` (required) and `max_results`.
- `firecrawl_fetch` — parameter `url`. Only added when a Firecrawl key is
  configured.

Anthropic's native `web_search` server tool (`type: web_search_20250305`) is
left in place when the mode is `enabled`; `injectAnthropicWebSearch` ensures
its type/max-uses are correct.

Because execution happens inside the bridge, **no `tool_use` blocks for the
search tools leak to the consumer** — the client receives only the model's
final text (or its own tools' calls).

## Invisible-to-consumer behavior

1. The consumer sends a request that includes a `web_search` tool (or
   `web_search_preview`).
2. The bridge strips the client tool and injects `tavily_search` /
   `firecrawl_fetch` (as Core tools for the Core path, or wire tools for the
   chat/google loops).
3. The model calls the injected tools; the bridge executes them and feeds the
   results back.
4. The bridge returns the final answer. Search tool calls and results are never
   part of the client-visible response.

## How to verify

1. **Config check.** Read the resolved settings from the management API (keys
   masked):

   ```bash
   curl -s -H "Authorization: Bearer $TOKEN" http://127.0.0.1:38440/api/v1/web-search
   ```

   Or start the bridge and look for the startup log line
   `injected web search enabled` / `web search injection mode enabled` for the
   provider.

2. **Live request.** Send a request with a `web_search` tool through
   `/v1/messages` or `/v1/chat/completions` and ask for something that requires
   fresh facts (for example today's news). The answer should contain current
   information, and the response must contain **no `tool_use` blocks** for
   `tavily_search` / `firecrawl_fetch`.

3. **Logs.** With debug logging, the Core loop logs
   `core search loop round` with the executed tool count; failed executions log
   `core search execution failed`.

## Related

- [CONFIGURATION.md](CONFIGURATION.md) — the `web_search` YAML section.
- [ARCHITECTURE.md](ARCHITECTURE.md#server-side-web-search-injection) — where
  injection sits in the request path.
- [EXTENSIONS.md](EXTENSIONS.md) — the `websearchinjected` extension.
