# API Reference

Provider Bridge exposes one HTTP surface with three groups of endpoints:

- **Inbound (model) endpoints** — what AI consumers speak: OpenAI Responses,
  Anthropic Messages, OpenAI Chat Completions, and model listing. These are
  registered in `internal/service/server/server.go` (`New`, ~line 190).
- **Management API** — JSON CRUD/status/config endpoints under `/api/v1/`,
  registered in `internal/service/api/router.go` (`registerRoutes`).
- **Embedded Web Console** — the compiled UI served under `/console/`.

Both `/v1/...` and the legacy no-`/v1` aliases (`/responses`, `/models`) are
registered for the model endpoints.

## Base information

| Item | Value |
|------|-------|
| Default listen address | `127.0.0.1:38440` (`config.DefaultAddr`) |
| Content type (JSON) | `application/json` |
| Content type (streaming) | `text/event-stream` |
| Content type (config export) | `application/x-yaml` |
| Docker/homelab base URL | `http://mini:38440` (tunnel: `http://localhost:38440`) |

## Authentication

Set `server.auth_token` in the config to require a bearer token. When the token
is non-empty, every request must send:

```http
Authorization: Bearer <server.auth_token>
```

Rules (implemented in `(*Server).ServeHTTP` in `server.go` and in the
management-API `AuthMiddleware` in `internal/service/api/helpers.go`):

- The `/console/` and `/console` asset paths are **exempt** from auth.
- Missing or wrong token on an inbound endpoint returns `401` with the
  OpenAI-shaped error body (`type: "authentication_error"`,
  `code: "invalid_auth"`).
- On `/api/v1/*`, a missing/invalid token returns `401` with the management
  error body `{"error":{"code":"invalid_auth","message":...}}`.
- When `persistence.active_provider` is not configured, the management API is
  not mounted at all (the `/api/v1/` handler is only registered when both
  `Runtime` and `Store` exist); when the store is unavailable the middleware
  returns `503 store_unavailable`.

## Session headers

Requests may identify a session so that cross-request state (reasoning replay,
usage session aggregation) is preserved. Keys are resolved in
`sessionKeyFromRequest` (`internal/service/server/session.go`), in this order:

| Header | Session key prefix |
|--------|--------------------|
| `Session_id` | `session:<value>` |
| `X-Codex-Window-Id` | `codex-window:<value>` |
| `X-Claude-Code-Session-Id` | `claude-session:<value>` |

With none of these headers the request gets an ephemeral session.

## Endpoint index

### Inbound endpoints

| Method | Path | Handler | Purpose |
|--------|------|---------|---------|
| POST | `/v1/responses` | `handleResponses` | OpenAI Responses API (streaming + non-streaming) |
| POST | `/responses` | `handleResponses` | Same, no-`/v1` alias |
| GET | `/v1/models` | `handleModels` | Model list (dual shape) |
| GET | `/models` | `handleModels` | Same, no-`/v1` alias |
| POST | `/v1/messages` | `handleAnthropicMessages` | Anthropic Messages API |
| POST | `/v1/messages/count_tokens` | `handleAnthropicCountTokens` | Token-count estimate |
| POST | `/v1/chat/completions` | `handleChatCompletions` | OpenAI Chat Completions |
| GET | `/console/` | `webui.Embedded()` | Embedded Web Console |

Non-`POST` requests to the POST-only endpoints return `405`; non-`GET` requests
to `/v1/models` return `405` (`method_not_allowed`).

Model bodies are covered in the sections below; the full management-API table
is in [Management API](#management-api).

---

## POST /v1/responses

OpenAI Responses-compatible chat/completion endpoint — the original inbound
protocol (default Codex consumer). Requests are converted to the Core IR and
dispatched to the resolved upstream provider; Responses-upstream providers are
proxied through raw.

### Request fields

| Field | Type | Notes |
|-------|------|-------|
| `model` | string | Route alias or `provider/model`/`model(provider)` reference |
| `input` | string \| array | Input text or message array |
| `instructions` | string | System/instructions text |
| `tools` | array | Tool definitions (including `web_search`) |
| `tool_choice` | object \| string | Tool selection strategy |
| `max_output_tokens` | number | Output token cap |
| `temperature`, `top_p` | number | Sampling |
| `stop` | string \| array | Stop sequences |
| `stream` | boolean | Enable SSE streaming |
| `include` | array | Extra response fields (e.g. reasoning) |
| `reasoning` | object | Reasoning config (e.g. `{"effort":"high"}`) |
| `store`, `previous_response_id`, `metadata`, `prompt_cache_key` | — | Passthrough |

### Non-streaming response

```json
{
  "id": "resp_xxx",
  "object": "response",
  "status": "completed",
  "model": "deepseek/deepseek-v4-pro",
  "output": [
    {"type": "message", "role": "assistant",
     "content": [{"type": "output_text", "text": "Hello!"}]}
  ],
  "usage": {"input_tokens": 10, "output_tokens": 42, "total_tokens": 52}
}
```

### Streaming event lifecycle

With `"stream": true` the response is SSE. Each event has an `event:` name and
a `data:` JSON payload; the payload repeats its own `type`. The events emitted
by the OpenAI client adapter (`internal/protocol/openai/adapter.go`) are:

```
response.created
response.in_progress
response.output_item.added
response.content_part.added
response.output_text.delta
response.output_text.done
response.content_part.done
response.output_item.done
response.reasoning_summary_part.added
response.reasoning_summary_text.delta
response.reasoning_summary_part.done
response.function_call_arguments.delta
response.function_call_arguments.done
response.completed
response.incomplete
response.failed
```

Example slice:

```
event: response.created
data: {"type":"response.created","response":{"id":"resp_x","status":"in_progress"}}
event: response.output_text.delta
data: {"type":"response.output_text.delta","delta":"Hello"}
event: response.completed
data: {"type":"response.completed","response":{"status":"completed","usage":{...}}}
```

---

## GET /v1/models

Returns the model catalog in **two shapes in one body**:

- `models[]` — the legacy Provider Bridge shape used by the embedded console.
- `object` + `data[]` — the standard OpenAI shape `{"object":"list","data":[...]}`
  read by OpenAI-shaped consumers (e.g. Open WebUI).

```json
{
  "object": "list",
  "data": [
    {"id": "mistral/zai-glm-5-3", "object": "model", "owned_by": "mistral", "name": "zai-glm-5-3"}
  ],
  "models": [
    {"slug": "mistral/zai-glm-5-3", "name": "zai-glm-5-3", "provider": "mistral"},
    {"slug": "claude-sonnet", "name": "Claude Sonnet", "provider": "anthropic", "model": "claude-sonnet-4"}
  ]
}
```

Behavior (`handleModels` in `server.go`):

- Provider entries use the `provider/model` slug, which `ResolveModel` accepts
  directly. Route aliases appear as additional entries and carry a `model` key.
- `data[]` uses `id` = slug, `object` = `"model"`, `owned_by` = provider key.
- `data[]` is **deduplicated against route aliases**: because a Codex short
  alias can point at the same `(provider, upstream model)` as a slug entry,
  aliases are sorted by slug and skipped when their target is already present.
  This prevents every model appearing twice in consumer UIs.

> **Metadata note:** `/v1/models` does **not** expose `context_window` or
> `max_output_tokens`. Read model metadata from `GET /api/v1/models/{slug}`
> (see [Models](#models)).

---

## POST /v1/messages

Anthropic Messages-compatible endpoint (Claude Code). The handler is
`handleAnthropicMessages` (`internal/service/server/inbound_handlers.go`); the
wire adapter is `internal/protocol/anthropic/client_adapter.go`
(`ClientProtocolID = "anthropic-messages"`).

### Request fields

| Field | Type | Notes |
|-------|------|-------|
| `model` | string | Route alias or model reference |
| `max_tokens` | number | Required by the Anthropic wire shape |
| `system` | string \| block array | System prompt (polymorphic) |
| `messages` | array | `{role, content}`; `content` may be a string or block array |
| `tools` | array | Tool definitions |
| `tool_choice` | object | `{type, name}` |
| `temperature`, `top_p`, `top_k` | number | Sampling |
| `stop_sequences` | array | Stop sequences |
| `stream` | boolean | Enable SSE streaming |
| `thinking` | object | e.g. `{"type":"adaptive"}` (mapped to the provider default) or `{"type":"enabled","budget_tokens":N}` |
| `output_config` | object | `{effort}` reasoning effort |
| `cache_control` | object | Prompt-cache control |
| `metadata` | object | Passthrough |

Wire normalization: `system` and message `content` accept either a string or a
block array. Tool results may arrive inside `user` messages; the adapter maps
them to the canonical Core `tool` role.

### Non-streaming response

```json
{
  "id": "msg_xxx",
  "type": "message",
  "role": "assistant",
  "model": "claude-sonnet",
  "content": [{"type": "text", "text": "Hello!"}],
  "stop_reason": "end_turn",
  "usage": {"input_tokens": 10, "output_tokens": 42}
}
```

Content blocks use the Anthropic vocabulary: `text`, `thinking`
(with `signature`), `tool_use` (`id`/`name`/`input`), and `tool_result`
(`tool_use_id`/`content`). Usage carries `input_tokens`, `output_tokens`,
`cache_creation_input_tokens`, `cache_read_input_tokens`.

### Streaming event lifecycle

With `"stream": true` the SSE sequence is:

```
message_start
  → content_block_start
  → content_block_delta*      (text_delta | thinking_delta | input_json_delta | signature_delta)
  → content_block_stop
  ... (one start/delta/stop cycle per content block)
  → message_delta             (stop_reason + output usage)
  → message_stop
ping                          (keep-alive, emitted as needed)
error                         (mid-stream upstream failure)
```

Details from `FromCoreStream`:

- `message_start` carries a `MessageResponse` with `role: "assistant"`.
- `content_block_start.content_block.type` is `thinking`, `tool_use`, or `text`.
- Reasoning deltas emit `thinking_delta`; reasoning signature is emitted as a
  `signature_delta` immediately before `content_block_stop`.
- Tool arguments stream as `input_json_delta` (`partial_json`). The visual-path
  synthesizer may emit the complete arguments in a single `input_json_delta`.
- `message_delta` carries `delta.stop_reason` and `usage` (input+output).
- Errors use the Anthropic error shape `{"type":"error","error":{"type":...,"message":...}}`.

---

## POST /v1/messages/count_tokens

Returns a token-count estimate for an Anthropic-shaped request. Exact counting
is optional for clients; Claude Code falls back to its own estimate when this
endpoint is absent.

Request (only these fields are read):

```json
{"model": "claude-sonnet", "messages": [...], "system": "...", "tools": [...]}
```

Response:

```json
{"input_tokens": 1234}
```

The estimate is `(len(messages) + len(system) + len(tools)) / 4` over the raw
JSON byte lengths (`handleAnthropicCountTokens`).

---

## POST /v1/chat/completions

OpenAI Chat Completions-compatible endpoint (LibreChat, Affiora). Handler:
`handleChatCompletions`; adapter: `internal/protocol/chat/client_adapter.go`
(`ClientProtocolID = "chat-completions"`).

### Request fields

| Field | Type | Notes |
|-------|------|-------|
| `model` | string | Required |
| `messages` | array | Required; `role` in `system`/`developer`/`user`/`assistant`/`tool` |
| `max_tokens` / `max_completion_tokens` | number | Legacy `max_tokens` is normalized to `max_completion_tokens` |
| `tools` | array | `{"type":"function","function":{...}}` |
| `tool_choice` | object \| string | Parsed to Core mode; raw preserved |
| `stream` | boolean | Enable SSE streaming |
| `stream_options` | object | `{"include_usage": true}` adds usage to the final chunk |
| `temperature`, `top_p`, `stop` | — | Sampling / stop sequences |
| `reasoning_effort` | string | `low`/`medium`/`high`/`minimal`; mapped to Core effort |
| `parallel_tool_calls`, `user`, `metadata` | — | Passthrough |

### Tool `arguments` are JSON strings

On the wire, Chat Completions tool-call arguments are **JSON strings**, not raw
objects:

```json
{"role":"assistant","tool_calls":[
  {"id":"call_1","type":"function","function":{"name":"get_weather","arguments":"{\"city\":\"Paris\"}"}}
]}
```

The adapter quotes Core tool input with `jsonQuote`; an empty argument set is
serialized as `""` (an empty `json.RawMessage` is invalid JSON and would be
dropped by the SSE writer).

### Non-streaming response

```json
{
  "id": "chatcmpl-xxx",
  "object": "chat.completion",
  "created": 1710000000,
  "model": "mistral/zai-glm-5-3",
  "choices": [
    {"index": 0, "message": {"role": "assistant", "content": "Hello!"}, "finish_reason": "stop"}
  ],
  "usage": {"prompt_tokens": 10, "completion_tokens": 42, "total_tokens": 52}
}
```

`usage.prompt_tokens_details.cached_tokens` is populated when the upstream
reports cached input tokens.

### Streaming chunk shape

Each SSE `data:` line is a `chat.completion.chunk`:

```
data: {"id":"chatcmpl-x","object":"chat.completion.chunk","created":1710000000,"model":"...","choices":[{"index":0,"delta":{"role":"assistant","content":"Hel"},"finish_reason":""}]}
data: {"id":"chatcmpl-x","object":"chat.completion.chunk",...,"choices":[{"index":0,"delta":{"reasoning_content":"..."}}]}
data: {"id":"chatcmpl-x","object":"chat.completion.chunk",...,"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"call_1","type":"function","function":{"name":"get_weather","arguments":""}}]}}]}
data: {"id":"chatcmpl-x","object":"chat.completion.chunk",...,"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"arguments":"{\"city\""}}]}}]}
data: {"id":"chatcmpl-x","object":"chat.completion.chunk",...,"choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}
data: [DONE]
```

Behavior (`chatStreamLoop`):

- Delta fields: `content`, `reasoning_content`, and `tool_calls`. The `role`
  is set to `assistant` on the first chunk.
- `finish_reason` maps from the Core stop reason (`stop`, `length`,
  `tool_calls`, ...). Upstream errors emit a chunk with `finish_reason:"error"`.
- When `stream_options.include_usage` is set, the final chunk carries `usage`.
- Tool arguments stream incrementally as JSON-string fragments; the visual-path
  synthesizer may emit the complete arguments in one chunk.

---

## Error shapes

There are three error envelopes, one per inbound family plus the management API.

### OpenAI-shaped (Responses, Chat Completions, models, and top-level auth)

```json
{"error": {"message": "...", "type": "invalid_request_error", "code": "invalid_json"}}
```

### Anthropic-shaped (`/v1/messages`, `/v1/messages/count_tokens`)

```json
{"type": "error", "error": {"type": "invalid_request_error", "message": "..."}}
```

Anthropic error `type` values are mapped from the status code
(`inboundAnthropicErrorType`): `400 → invalid_request_error`,
`401 → authentication_error`, `404 → not_found_error`,
`413 → request_too_large`, `429 → rate_limit_error`, `5xx → api_error`.

### Management API

```json
{"error": {"code": "not_found", "message": "provider \"x\" does not exist"}}
```

| HTTP status | Typical cause |
|-------------|---------------|
| 400 | Invalid JSON, failed validation, bad config draft |
| 401 | Missing/invalid bearer token |
| 404 | Unknown model/route/provider, model resolution failure |
| 409 | `revisionConflict`, or deleting a model still referenced by a provider |
| 500 / 502 | Internal error / upstream provider error |
| 503 | Config store unavailable (`store_unavailable`) |

---

## Web Console

The production binary serves the embedded console at `/console/`
(`webui.Embedded()`, `internal/service/webui/embed.go`). The console uses the
same-origin APIs:

- `/api/v1/config/graph` — read the resource graph and save field edits.
- `/api/v1/logs/recent` and `/api/v1/logs/stream` — the Logs page.
- `/v1/models` and `/v1/responses` — the RPC smoke-test panel.

The console edits the config graph directly through UI fields; it does not
expose the config file path or a YAML editor. Secret fields are read-masked and
treated as write-only inputs.

---

## Management API

All endpoints below are relative to `/api/v1/` and are registered in
`internal/service/api/router.go`. They are only mounted when
`persistence.active_provider` is configured and the config store initializes.

List endpoints that return a page use the envelope
`{"data":[...],"total":N,"limit":L,"offset":O}`. Pagination query parameters:
`limit` (default `20`, max `100`) and `offset` (default `0`).

### Providers

| Method | Path | Purpose |
|--------|------|---------|
| GET | `/providers` | List providers (paginated) |
| GET | `/providers/{key}` | Provider detail (`api_key` masked) |
| PUT | `/providers/{key}` | Create a provider (stages a change) |
| PATCH | `/providers/{key}` | Update a provider (stages a change) |
| DELETE | `/providers/{key}` | Delete a provider (stages a change) |
| POST | `/providers/{key}/test` | Connectivity probe (5s timeout) |

`PUT`/`PATCH` body: `base_url`, `api_key`, `version`, `protocol`,
`user_agent`. `protocol` defaults to `anthropic` on create. A masked
`api_key` (`******`) on `PATCH` keeps the existing key. These handlers stage a
change and return `202 {"change_id":...,"status":"pending"}`; apply with
`POST /changes/apply`.

### Offers

| Method | Path | Purpose |
|--------|------|---------|
| POST | `/providers/{key}/offers` | Add a provider offer |
| PATCH | `/providers/{key}/offers/{model}` | Update an offer |
| DELETE | `/providers/{key}/offers/{model}` | Delete an offer |

Offer fields: `model`, `upstream_name`, `priority`, `input_price`,
`output_price`, `cache_write`, `cache_read`.

### Models

| Method | Path | Purpose |
|--------|------|---------|
| GET | `/models` | List model definitions (paginated; `slug`, `display_name`, `context_window`, `providers`) |
| GET | `/models/{slug}` | Model metadata (`context_window`, `max_output_tokens`, `input_modalities`, `providers`) |
| PUT | `/models/{slug}` | Create/update metadata (stages a change) |
| DELETE | `/models/{slug}` | Delete (409 if still referenced by a provider offer) |

Use `/models/{slug}` as the metadata source of truth — `/v1/models` omits
context window and output limits.

### Routes

| Method | Path | Purpose |
|--------|------|---------|
| GET | `/routes` | List route aliases (paginated) |
| GET | `/routes/{alias}` | Route detail (`model`, `provider`, `display_name`, `context_window`) |
| PUT | `/routes/{alias}` | Create/update a route (stages a change) |
| DELETE | `/routes/{alias}` | Delete a route (stages a change) |

### Settings

| Method | Path | Purpose |
|--------|------|---------|
| GET | `/defaults` | Default model / max tokens / system prompt |
| PUT | `/defaults` | Update defaults (stages a change) |
| GET | `/web-search` | Web-search settings (API keys masked) |
| PUT | `/web-search` | Update web-search settings (stages a change) |
| GET | `/extensions` | List extension names |
| GET | `/extensions/{name}` | Extension JSON config |
| PUT | `/extensions/{name}` | Update extension config (stages a change) |

> Web-search support resolution is startup-only: after applying a web-search
> change, restart the container for it to take effect. See
> [WEB-SEARCH.md](WEB-SEARCH.md).

### Config

| Method | Path | Purpose |
|--------|------|---------|
| GET | `/config/graph` | Current editable resource graph |
| PATCH | `/config/graph` | Apply field changes to the graph |
| POST | `/config/graph/validate` | Validate graph changes without committing |
| POST | `/config/resources/{kind}` | Create a config resource |
| DELETE | `/config/resources/{kind}/{id}` | Delete a config resource |
| GET | `/config/effective` | Effective config with secrets masked |
| GET | `/config/export` | Export YAML |
| POST | `/config/import` | Parse YAML and stage changes |
| POST | `/config/validate` | Validate a YAML string |

### Changes

| Method | Path | Purpose |
|--------|------|---------|
| GET | `/changes` | List pending changes |
| POST | `/changes/apply` | Apply pending changes and reload |
| POST | `/changes/discard` | Discard pending changes |

### Status, stats, logs, version

| Method | Path | Purpose |
|--------|------|---------|
| GET | `/status` | Version, mode, provider/route counts, address |
| GET | `/status/providers` | Per-provider status (protocol, base URL, offer count, health) |
| GET | `/sessions` | Active sessions (keys partially masked) |
| GET | `/stats` | In-memory stats summary |
| GET | `/stats/summary` | Requests, tokens, cache hit rate, cost, duration |
| GET | `/stats/usage` | Usage totals + per-model rows |
| GET | `/logs` | Alias of `/logs/recent` |
| GET | `/logs/recent` | Recent backend log records |
| GET | `/logs/stream` | Follow logs as SSE |
| GET | `/version` | Version, build time, Go version |

`/stats/usage` accepts `range` (`session` default, `24h`, `7d`, `30d`, `all`)
or explicit `since`/`until` RFC3339 timestamps. Cross-session ranges aggregate
from persisted metrics when a usage source is registered, otherwise they fall
back to the in-memory session stats.

Response shape:

```json
{
  "totals": {"requests": 42, "input_tokens": 100, "output_tokens": 200,
             "cache_read": 0, "cache_hit_rate": 0.0, "total_cost": 0.01, "duration": "1m0s"},
  "by_model": [
    {"model": "claude-sonnet", "actual_model": "claude-sonnet-4", "requests": 42,
     "input_tokens": 100, "output_tokens": 200, "cost": 0.01, "avg_cost_per_mtoken": 33.3}
  ]
}
```

### Logs API

`GET /api/v1/logs/recent?limit=200` returns an array of records. `limit`
defaults to `100` and is capped at `1000`. Each record keeps the backend's raw
log text:

```json
[
  {"timestamp":"2026-06-07T00:00:00Z","level":"INFO","message":"request completed",
   "attrs":{"model":"claude-sonnet"},"raw":"time=... level=INFO msg=request-completed"}
]
```

`GET /api/v1/logs/stream` returns `text/event-stream`; each event is one JSON
record:

```text
data: {"timestamp":"...","level":"INFO","message":"...","raw":"..."}
```

### Config graph API

`GET /api/v1/config/graph` returns the editable resource graph:

```json
{
  "revision": "rev-...",
  "resources": [
    {
      "kind": "defaults",
      "id": "main",
      "label": "Defaults",
      "value": {"model": "claude-sonnet", "max_tokens": 65536},
      "schema": {"fields": [{"path": "model", "type": "string", "label": "Model", "hotReloadable": true}]},
      "status": "saved",
      "runtimeImpact": "normal",
      "hotReloadable": true
    }
  ],
  "validation": {"valid": true},
  "runtime": {"status": "ok"},
  "capabilities": {"autosave": true, "logs": true}
}
```

Resource kinds (`internal/service/configgraph/types.go`): `mode`, `trace`,
`log`, `server`, `defaults`, `model`, `provider`, `provider_offer`, `route`,
`web_search`, `cache`, `persistence`, `extension`, `proxy`.

Each resource carries its own `schema.fields[]` (path, type, label, required,
secret, control, enum, hotReloadable, runtimeImpact). Secret fields are masked
with `******` (`configgraph.secretMask`) in graph reads; the `server` resource
masks `auth_token`, providers mask `api_key`, web search masks
`tavily_api_key`/`firecrawl_api_key`, and the proxy masks nested `api_key`
fields.

`PATCH /api/v1/config/graph` body:

```json
{
  "baseRevision": "rev-...",
  "changes": [
    {"kind": "defaults", "id": "main", "field": "model", "value": "claude-sonnet"}
  ]
}
```

Result table (`PatchResponse.result`):

| `result` | HTTP status | Meaning |
|----------|-------------|---------|
| `committed` | 200 | Committed and graph rebuilt |
| `restartRequired` | 200 | Committed but needs a restart to fully apply |
| `revisionConflict` | 409 | `baseRevision` is not the current revision |
| `validationRejected` | 400 | Candidate config failed static validation |
| `runtimeRejected` | 400 | Passed static validation but runtime reload rejected it (may include `rollbackValue`) |
| `draftRejected` | 400 | The change draft could not be applied to the config structure |

`POST /api/v1/config/graph/validate` uses the same request/response shape but
does not commit; on success it returns the candidate `graph` with the revision
unchanged.

Create a resource:

```http
POST /api/v1/config/resources/{kind}
Content-Type: application/json

{"baseRevision": "rev-...", "id": "new-id", "value": {}}
```

Delete a resource (base revision via query or JSON body):

```http
DELETE /api/v1/config/resources/{kind}/{id}?baseRevision=rev-...
```

Unknown JSON fields are rejected (`decodeStrictJSON`).

`POST /api/v1/config/validate` body is `{"config": "<yaml string>"}`; a
validation failure may still return HTTP `200` with
`{"valid": false, "errors": ["..."]}`.

### Config export

Plaintext secrets require both a query flag and an explicit header:

```http
GET /api/v1/config/export?include_secrets=true
X-Confirm-Secrets: true
```

Without the header the export returns `400 confirmation_required`. Without
`include_secrets=true` the export masks secrets. The response is
`application/x-yaml` with a `Content-Disposition: attachment` filename.

---

## Codex CLI integration

Point Codex at the bridge (any non-empty API key when auth is configured):

```toml
[openai]
base_url = "http://127.0.0.1:38440/v1"
api_key = "any-non-empty-value"
```

The bridge resolves the model alias, injects server-side web search if
configured, and converts to the resolved upstream protocol automatically. See
[CONSUMERS.md](CONSUMERS.md) for the full consumer matrix.
