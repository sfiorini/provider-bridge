# Configuration

> The annotated, up-to-date example is [`../config.example.yml`](../config.example.yml).
> A JSON Schema can be generated with `providerbridge -dump-config-schema`; it is
> written as `config.schema.json` next to the resolved config file (the file is
> not checked in).

Provider Bridge reads a YAML config file. When `-config` is omitted the default
path is `$HOME/provider-bridge/config.yml`. If that file does not exist, the
binary writes a starter config (SQLite enabled, database at
`$HOME/provider-bridge/data/provider-bridge.db`) and continues to start.

Passing `-config <path>` is explicit: the file must already exist, otherwise the
process fails fast with a startup diagnostic.

## Live config: the SQLite graph vs. `config.yml`

The **SQLite config graph is the live source of truth** once persistence is
enabled. `config.yml` is the *seed* (read at startup) and the *mirror* (written
by tooling such as the Codex catalog generator). The two are related but serve
different jobs:

- The management API and Web Console read and patch the graph
  (`/api/v1/config/graph`). Changes are persisted to the graph immediately;
  the YAML file is **not** automatically rewritten.
- `config.yml` is for CLI startup, deployment scripts, backups, migrations, and
  bulk admin edits. On boot, the file is loaded; the graph is the runtime store
  thereafter.
- To keep the file in sync after editing through the Console, export the graph
  and write it back to `config.yml` (or edit the file and restart).

The Web Console maps the current config to graph **resources**:

| Console page | Primary resources |
|--------------|-------------------|
| Overview | `mode`, runtime state, validation state, restart requirements |
| Models & Providers | `provider`, `provider_offer`, `model` |
| Routes | `route` |
| Defaults | `defaults`, `trace`, `log` |
| Search & Tools | `web_search`, `extension`, `proxy` |
| Storage | `cache`, `persistence` |
| Security | `server` |
| Logs | Backend log output only (does not modify config) |

Field edits are sent as graph patches. Ordinary hot-reloadable fields take
effect when the patch is committed; `server`, `mode`, `proxy` and `persistence`
are marked as requiring a restart. Secret fields are masked as `******` on
read, and the Console treats them as write-only inputs (submitting the literal
mask leaves the stored value unchanged).

To use the Console you normally enable persistent config storage, for example
SQLite:

```yaml
persistence:
  active_provider: db_sqlite

extensions:
  db_sqlite:
    enabled: true
    config:
      path: ~/.provider-bridge/provider-bridge.db
      wal: true
      busy_timeout_ms: 5000
      max_open_conns: 1
```

## Top-level structure

```yaml
mode: "Transform"  # Transform / CaptureAnthropic / CaptureResponse

trace:
  enabled: false   # dump full request/response traces to data/trace/

log:
  level: "info"    # debug / info / warn / error / off
  format: "text"   # text / json

server:
  addr: "127.0.0.1:38440"
  auth_token: ""          # bearer token; empty = no auth
  max_sessions: 100       # 0 = unlimited
  session_ttl: "24h"      # idle session timeout

defaults:
  model: "provider-bridge"
  max_tokens: 65536
  system_prompt: ""       # prepended to every request

egress_proxy: "http://127.0.0.1:7890"  # optional outbound proxy

web_search:
  support: "auto"         # auto / enabled / disabled / injected
  tavily_api_key: "tvly-..."
  firecrawl_api_key: "fc-..."
  search_max_rounds: 5

cache:
  mode: "explicit"        # off / automatic / explicit / hybrid
  ttl: "5m"

persistence:
  active_provider: db_sqlite  # db_sqlite / db_d1

extensions: {}
models: {}
providers: {}
routes: {}
```

## Mode

| Value | Behavior |
|-------|----------|
| `Transform` | Accept inbound requests and translate them to the provider's protocol before forwarding. |
| `CaptureAnthropic` | Transparent proxy to an Anthropic upstream (no translation). |
| `CaptureResponse` | Transparent proxy to an OpenAI upstream (no translation). |

Capture modes require the matching `proxy` block below.

## Server

```yaml
server:
  addr: "127.0.0.1:38440"    # listen address
  auth_token: ""             # Bearer token for all endpoints (empty = no auth)
  max_sessions: 100          # maximum tracked sessions, 0 = unlimited
  session_ttl: "24h"         # idle timeout before a session is evicted
```

Sessions are keyed by the `Session_id` / `X-Codex-Window-Id` /
`X-Claude-Code-Session-Id` headers; requests without one use an ephemeral
session. `max_sessions` caps how many are retained and `session_ttl` is the
idle eviction window (default `24h`).

## Defaults

```yaml
defaults:
  model: "provider-bridge"   # fallback model alias when a request names none
  max_tokens: 65536          # fallback max output tokens
  system_prompt: ""          # optional prompt prepended to every request
```

## Tracing

```yaml
trace:
  enabled: false   # dump full request/response traces to data/trace/
```

The legacy top-level `trace_requests: true` key is still accepted and maps to
`trace.enabled`.

## Logging

```yaml
log:
  level: "info"    # debug / info / warn / error / off
  format: "text"   # text / json
```

## Egress proxy

```yaml
egress_proxy: "http://127.0.0.1:7890"
```

When set, every upstream API call is made through this proxy. The bridge builds
a proxy-aware HTTP client (`http.ProxyURL`) from the value, so `http`, `https`
and `socks5` URLs are accepted. Leave empty for direct egress.

## Models

Model definitions hold shared metadata used by routing, the Codex catalog and
usage accounting. `models.<slug>` is the canonical metadata; providers refer to
it from their `offers`.

```yaml
models:
  my-model:
    context_window: 1000000
    max_output_tokens: 384000
    display_name: "My Model"
    description: "Short catalog description"
    base_instructions: "Extra system instructions for the catalog"
    supports_reasoning: true
    default_reasoning_level: "high"
    supported_reasoning_levels:
      - effort: "low"
        description: "Low effort reasoning"
      - effort: "medium"
        description: "Medium effort reasoning"
      - effort: "high"
        description: "High effort reasoning"
      - effort: "xhigh"
        description: "Extra high effort reasoning"
    supports_reasoning_summaries: true
    default_reasoning_summary: "auto"
    input_modalities:
      - "text"
      - "image"
    supports_image_detail_original: false
    web_search:
      support: "auto"     # auto / enabled / disabled / injected
    extensions:
      deepseek_v4:
        enabled: true
      visual:
        enabled: true
```

- `input_modalities` lists accepted modalities. It feeds the Codex
  `models_catalog.json`; an empty list defaults to `["text"]`. Use `image` for
  vision-capable models.
- `supports_image_detail_original` declares whether the model accepts
  uncompressed ("original") image detail in the Codex catalog.
- `context_window` / `max_output_tokens` must match the deployed model; the
  verified values live in [INVENTORY.md](../INVENTORY.md).

## Providers

Providers describe how to reach an upstream API and which protocol it speaks.

```yaml
providers:
  my-provider:
    protocol: "anthropic"          # anthropic | openai-response | google-genai | openai-chat
    base_url: "https://api.example.com"
    api_key: "sk-..."
    version: "2023-06-01"
    user_agent: "provider-bridge/1.0"

    # Google GenAI-only fields (protocol: google-genai)
    project: "my-gcp-project"
    location: "us-central1"
    api_version: "v1"

    web_search:
      support: "auto"
      max_uses: 1
      tavily_api_key: "tvly-..."
      firecrawl_api_key: "fc-..."
      search_max_rounds: 3

    offers:
      - model: my-model
        pricing:
          input_price: 2
          output_price: 8
          cache_write_price: 1
          cache_read_price: 0.25
```

### Multiple API keys (accounts) per provider

`api_key` accepts **multiple keys separated by commas**:

```yaml
providers:
  my-provider:
    api_key: "sk-first,sk-second,sk-third"
```

- The **first** key has precedence; a value without commas is a single key
  and behaves exactly as before.
- On HTTP **429** (rate limited) or **402** (quota exhausted) the bridge
  retries the *same request* with the next key, immediately (no backoff).
- The key that succeeds becomes the **active** key and is remembered across
  restarts (persisted in the `key_rotation` table of the config graph — it is
  not part of `config.yml`).
- After a full rotation (every key returned 429/402) the active key's error
  is surfaced as before; mid-stream failures are not retried.
- Rotation covers every model-upstream call path (Core executor,
  Responses-inbound adapter path, web-search loops, and the raw OpenAI
  Responses passthrough). Google-genai upstreams are excluded.
- Masking is unchanged: the whole value is masked (`******`) in graph reads
  and exports, and `PATCH` with `"******"` keeps the existing value.

See [COOKBOOK.md](COOKBOOK.md#9-add-a-second-api-key-account-for-rate-limit-headroom)
for a worked recipe.

### Protocol values

| Value | Upstream format | Adapter package |
|-------|-----------------|-----------------|
| `anthropic` (default) | Anthropic Messages API | `internal/protocol/anthropic` |
| `openai-response` | OpenAI Responses API | `internal/protocol/openai` (passthrough) |
| `google-genai` | Google Generative AI (Gemini) API | `internal/protocol/google` |
| `openai-chat` | OpenAI Chat Completions API | `internal/protocol/chat` |

`anthropic`, `openai-response` and `openai-chat` are supported by the shared
Core upstream executor. `google-genai` is wired in the original Responses
dispatch; the Core executor returns a clear error for it today.

### Offers and pricing

Each provider declares the models it serves as `offers`. `model` is the shared
slug from `models`; `upstream_name` renames it on the wire; `priority` breaks
ties when several providers offer the same slug. Per-provider pricing lives on
the offer:

```yaml
offers:
  - model: my-model
    upstream_name: "my-model-2026-01"
    priority: 1
    pricing:
      input_price: 2.0        # per million tokens
      output_price: 8.0
      cache_write_price: 1.0
      cache_read_price: 0.25
```

Pricing feeds usage statistics and cost reporting. When a provider does not
charge, omit `pricing`.

## Routes

Routes are optional friendly aliases. Provider models can also be addressed
directly as `model(provider)`. A route maps a client-facing alias to a model
slug plus provider:

```yaml
routes:
  alias-name:            # model name clients use
    model: my-model      # slug defined under models:
    provider: my-provider
```

Backward-compatible shorthand `<alias>: "provider/upstream-model"` is also
accepted and parsed into `model` + `provider`.

## Web search

Web-search support can be set globally, per provider, per model and per route
(most specific wins: route > model > provider > global).

| Mode | Behavior |
|------|----------|
| `auto` | Use the provider's native web-search API when available, otherwise fall back to injected mode. |
| `enabled` | Use the provider's native web search. |
| `disabled` | Disable web search. |
| `injected` | Inject `tavily_search` / `firecrawl_fetch` tools and execute them inside the bridge. |

`tavily_api_key`, `firecrawl_api_key` and `search_max_rounds` resolve the same
way (model wins over provider wins over global; default `search_max_rounds` is
`5`). Resolution is **startup-only** — changing any web-search setting requires
a restart. See [WEB-SEARCH.md](WEB-SEARCH.md) for the execution loops and
per-protocol behavior.

## Cache

```yaml
cache:
  mode: "explicit"              # off / explicit / automatic / hybrid
  ttl: "5m"
  prompt_caching: true
  automatic_prompt_cache: false
  explicit_cache_breakpoints: true
  allow_retention_downgrade: false
  max_breakpoints: 4
  min_cache_tokens: 1024
  expected_reuse: 2
  minimum_value_score: 2048
  min_breakpoint_tokens: 1024
```

## Persistence

```yaml
persistence:
  active_provider: db_sqlite    # db_sqlite (local) / db_d1 (Cloudflare edge)
```

The persistence provider backs the config graph, metrics and other database
consumers. `db_sqlite` is the local default; `db_d1` is for Cloudflare Workers
deployments. The database provider itself is configured under `extensions`.

## Extensions

Extensions are configured as `extensions.<name>.enabled` (scope-resolved from
global/provider/model/route) plus a `config:` block owned by the extension.

```yaml
extensions:
  deepseek_v4:
    enabled: true
    config:
      reinforce_instructions: true
  visual:
    enabled: true
    config:
      provider: "kimi"
      model: "kimi-for-coding"
      max_rounds: 4
      max_tokens: 2048
  kimi_workaround:
    enabled: true
    config:
      max_tool_rounds: 50
      convergence_margin: 0.8
  codex_tool_proxy:
    enabled: true
  db_sqlite:
    enabled: true
    config:
      path: ./data/provider-bridge.db
      wal: true
      busy_timeout_ms: 5000
      max_open_conns: 1
  metrics:
    enabled: true
    config:
      default_limit: 100
      max_limit: 1000
```

The **visual** extension injects `visual_brief` / `visual_qa` tools and routes
image analysis to a configured vision provider. Its `provider`/`model` point at
a normal provider entry and are resolved at runtime; `input_modalities`
declared on the target model determine whether it is eligible to receive
images. See [EXTENSIONS.md](EXTENSIONS.md) for every shipped extension.

## Proxy (Capture modes only)

Required when `mode` is `CaptureResponse` or `CaptureAnthropic`:

```yaml
proxy:
  response:
    base_url: "https://api.openai.com"
    api_key: "sk-..."
    model: "gpt-5.5"          # default model for /v1/responses pass-through
  anthropic:
    base_url: "https://api.anthropic.com"
    api_key: "sk-..."
    version: "2023-06-01"
```

## CLI flags

| Flag | Default | Description |
|------|---------|-------------|
| `-config` | `$HOME/provider-bridge/config.yml` | Path to the config file |
| `-addr` | from config | Override the listen address |
| `-mode` | from config | Override the mode (`Transform` / `CaptureAnthropic` / `CaptureResponse`) |
| `-print-addr` | — | Print the configured listen address and exit |
| `-print-mode` | — | Print the configured mode and exit |
| `-print-default-model` | — | Print the default model alias and exit |
| `-print-codex-model` | — | Print the configured Codex model and exit |
| `-print-claude-model` | — | Print the configured Claude Code model and exit |
| `-print-codex-config <model>` | — | Generate Codex `config.toml` for the model and exit |
| `-codex-base-url` | — | Base URL written into the generated Codex config |
| `-codex-home` | — | `CODEX_HOME` directory; when set, also writes `models_catalog.json` |
| `-dump-config-schema` | — | Generate `config.schema.json` alongside the config and exit |
