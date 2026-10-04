# AGENTS.md — Provider Bridge

**Read this first.** This file is the project's institutional memory: what
Provider Bridge is, where it came from, everything that was changed and why,
how it works inside, and how to develop against it. Any AI agent (or human)
starting work in this repo should treat this as the authoritative overview
alongside `PATCHES.md` (deployment runbook) and the code itself.

---

## 1. What Provider Bridge is

**Provider Bridge is a self-hosted, multi-protocol AI model gateway.** One
process, one config, one token — and every AI consumer in the homelab talks to
every model provider through it:

- Change a provider (add/remove/rotate keys, swap models, adjust pricing)
  **in one place** and every consumer sees the change.
- Every consumer speaks its **native wire protocol**; the bridge translates
  all of them through one internal representation ("Core") to any upstream
  provider protocol.

Inbound (what consumers speak)          →  Core  →  Upstream (what providers speak)
- OpenAI **Responses API** (`/v1/responses`)            anthropic
- **Anthropic Messages API** (`/v1/messages`)           openai-chat
- OpenAI **Chat Completions** (`/v1/chat/completions`)  google-genai
- Model listing (`/v1/models`, both OpenAI and legacy shapes)

Feature set: model alias/route resolution, server-side **web search
injection** (tavily + firecrawl executed inside the bridge, invisible to the
consumer), visual orchestration for image-capable models, DeepSeek reasoning
replay caching, per-model pricing/usage stats, tracing, an embedded web
console, and a persistent SQLite config graph manageable via a management API.

### Current consumers (homelab deployment on "mini")

| Consumer | Wire | Notes |
|---|---|---|
| Codex (OpenAI) | Responses | default consumer since the moon-bridge days |
| Claude Code | Anthropic Messages | opus→mistral/zai-glm-5-3, sonnet→deepseek/deepseek-v4-pro, haiku→zen/space-bunny-free |
| LibreChat | Chat Completions | single "Provider Bridge" endpoint, model list fetched live |
| Open WebUI | OpenAI type, api_type `responses` | reads `/v1/models` OpenAI shape |

---

## 2. Origin and history

**Provider Bridge started as a fork of [moon-bridge](https://github.com/ZhiYi-R/moon-bridge)
(ZhiYi-R) at upstream commit `c9ae8a8` (October 2026).** Moon-bridge is a
Codex-oriented proxy whose inbound surface is the OpenAI Responses API only,
with upstream providers for DeepSeek (Anthropic protocol) and OpenAI-compatible
endpoints. It was deployed to serve Codex with Mistral-hosted models
(including Z.ai GLM via Mistral's API) and worked well, but it spoke exactly
one inbound protocol — so every other consumer (Claude Code, LibreChat,
Open WebUI, anything chat-completions-shaped) needed direct provider
credentials and per-app configuration. That defeated the point of a gateway.

The project has since **veered from "patched proxy" toward a complete,
standalone product**: a universal protocol bridge with its own inbound
protocol adapters, a protocol-agnostic upstream executor, and a consumer
matrix that spans every major coding/chat harness. The upstream moon-bridge
history is preserved in this repo's git log (the fork point is readable, all
divergence happens in commits after `c9ae8a8`); the plan is to keep pulling
useful upstream changes opportunistically while the product identity, inbound
surface, and architecture roadmap are our own.

### 2.1 The moon-bridge patch era (upstream + 5 local commits)

Moon-bridge upstream is DeepSeek/Anthropic-centric; these patches fixed
openai-chat + Codex interop and were carried as a patch series before the
fork merged them into `main`:

1. **Gate empty reasoning_content replay on provider** — moonbridge emitted
   `reasoning_content: ""` on replayed assistant tool-call messages for ALL
   chat providers (a DeepSeek-only requirement). Mistral rejects the field
   entirely (HTTP 422): every multi-turn Codex session with tool history
   failed. Now only emitted for providers that need it.
2. **Strip client-native web_search tools** — Codex sends a native
   `web_search` function tool (no parameters); Mistral reserves that tool
   name and 422s. Stripped in the Core→Chat conversion.
3. **Respect web_search support config for openai-chat** — for openai-chat
   providers the configured `web_search.support` was ignored at provider
   level and model level always resolved to disabled, so injected search
   could never be enabled.
4. **Execute injected search on the visual-orchestrator path** — the visual
   orchestrator bypassed the injected-search loops, so the model's
   `tavily_search`/`firecrawl_fetch` calls were forwarded to Codex, which
   cannot execute them. Adds a Core-level search loop for the visual path.
5. **OpenAI-compatible `data[]` in `/v1/models`** — the endpoint only
   returned the proprietary `{"models":[...]}` shape. OpenAI-shaped consumers
   (Open WebUI et al.) read `data[].id` from `{"object":"list","data":[...]}`
   and saw zero models. The response now carries both keys; `id` uses the
   `provider/model` slug (accepted directly by model resolution), and
   `data[]` is **deduplicated against route aliases** (codex short names
   shadow the same upstream models — without dedupe every model appeared
   twice in consumer UIs).

### 2.2 The inbound protocol era (the fork's own product work)

Implemented on top of the patch series, all merged into `main`:

- **Anthropic Messages inbound** (`internal/protocol/anthropic/client_adapter.go`):
  `POST /v1/messages` + `/v1/messages/count_tokens`. Claude Code-grade: full
  streaming SSE (message_start → content_block_start/delta/stop with
  text_delta/thinking_delta/input_json_delta/signature_delta → message_delta
  → message_stop, ping), thinking blocks with signatures, tool_use
  round-trips, polymorphic wire normalization (system and content as string
  or block array), Anthropic-shaped errors, `thinking:{"type":"adaptive"}`
  mapped to provider default.
- **OpenAI Chat Completions inbound** (`internal/protocol/chat/client_adapter.go`):
  `POST /v1/chat/completions`. Streaming chunks with delta
  content/reasoning_content/tool_calls, finish_reason mapping, `include_usage`,
  legacy `max_tokens` normalized to `max_completion_tokens`, tool arguments
  as JSON strings.
- **Shared Core-level upstream executor** (`internal/service/server/core_upstream.go`):
  both new inbounds convert to Core early, then one executor handles model
  resolution, web-search injection, visual orchestration, DeepSeek reasoning
  replay, session keying (including Claude Code's `X-Claude-Code-Session-Id`
  header), tracing and usage stats. The original Responses dispatch
  (`handleWithAdapters`/`handleAdapterStream`) is deliberately untouched —
  zero regression risk to the proven Codex path.
- **Upstream bug fixes** (found by end-to-end testing; these affected the
  existing Codex path too): the visual-path stream synthesizers dropped
  `tool_use` blocks entirely — tool calls silently vanished from synthesized
  streams on that path for every protocol.

### 2.3 Wire-level lessons (all cost real debugging time — do not regress)

These are the invariant rules the inbound adapters must respect:

1. **Tool results are Core role `"tool"`** — not `user`+tool_result. Anthropic
   carries tool results in user messages; Chat carries them as `role:"tool"`
   with `tool_call_id`; both upstream adapters expect the canonical Core role
   `tool` (the anthropic upstream adapter maps it back to user+tool_result;
   the chat adapter flattens `user`+tool_result into plain text, which
   upstreams reject with "Not the same number of function calls and
   responses").
2. **Chat Completions tool `arguments` are JSON STRINGS on the wire**
   (`"arguments":"{\"a\":1}"`), not raw objects. Upstream providers happen to
   accept objects, but clients parse strings.
3. **An empty `json.RawMessage` is invalid JSON.** Marshaling a chunk that
   contains one fails, and a "skip on marshal error" SSE writer silently
   drops the chunk — use `json.RawMessage(`""`)` for empty arguments.
4. **The visual-path synthesizer emits `CoreToolCallArgsDone`** (complete
   arguments in one event), not just incremental `CoreToolCallArgsDelta` —
   stream loops must handle both.
5. **Two synthesizers exist** (`coreResponseToStreamEvents` and
   `coreResponseToCoreStream`) — a fix to tool streaming must cover both.

---

## 3. Architecture

### 3.1 The Core format (`internal/format`)

The protocol-agnostic intermediate representation. Key types:

- `CoreContentBlock` — one flattened struct, `Type` discriminator:
  `"text"`, `"image"` (ImageData+MediaType), `"tool_use"`
  (ToolUseID/ToolName/ToolInput), `"tool_result"` (ToolUseID +
  ToolResultContent), `"reasoning"` (ReasoningText+ReasoningSignature).
- `CoreMessage` — Role + Content blocks. System lives on the request
  (`CoreRequest.System`), not in messages.
- `CoreRequest` / `CoreResponse` — the full request/response; `CoreRequest`
  carries Tools, ToolChoice (Mode+Name+Raw for lossless round-trip),
  Thinking, Output (effort), sampling params, and `Extensions`
  (per-protocol passthrough, e.g. `Extensions["openai"]["reasoning"]["effort"]`
  for the openai-chat upstream).
- `CoreStreamEvent` — the stream event union with `CoreStreamEventType`
  lifecycle (created/in_progress/completed/incomplete/failed),
  content-block lifecycle (started/delta/done), text deltas, tool args
  deltas, item add/done, ping. Reasoning deltas are `CoreTextDelta` events
  whose `ContentBlock.Type == "reasoning"`.

### 3.2 Adapters and the registry (`internal/format/adapter.go`, `internal/service/app/app.go`)

Everything converts through Core via four interfaces:

- `ClientAdapter` (inbound): `ToCoreRequest` / `FromCoreResponse`
- `ClientStreamAdapter` (inbound streaming): `FromCoreStream` → a
  protocol-specific stream result (channel + buffer for trace)
- `ProviderAdapter` (upstream): `FromCoreRequest` / `ToCoreResponse`
- `ProviderStreamAdapter` (upstream streaming): `ToCoreStream` →
  `StreamResult{Events <-chan CoreStreamEvent, StreamBuffer}`

All registered in a `format.Registry` and wired in
`internal/service/app/app.go` (~line 250): inbound `openai-response`
(OpenAIAdapter), `anthropic-messages` (AnthropicClientAdapter),
`chat-completions` (ChatClientAdapter); upstream `anthropic`, `openai-chat`,
`google-genai`. Upstream adapters return **Core** — the server-side
per-protocol switch exists only to type-assert the produced wire request and
pick the right HTTP client for the provider.

### 3.3 Request paths (`internal/service/server/`)

- `dispatch.go` `handleResponses` — routes `/v1/responses`: OpenAI-response
  upstream → raw passthrough proxy; everything else → `handleWithAdapters`.
- `adapter_dispatch.go` `handleWithAdapters`/`handleAdapterStream` — the
  original Responses-inbound dispatch (Core in, OpenAI wire out). Kept
  untouched by policy.
- `core_upstream.go` `executeCoreUpstream` — the shared upstream half for
  the new inbounds: Core in → CoreResponse / Core event stream out.
  Supports anthropic + openai-chat upstreams; google-genai returns a clear
  error (no provider configured uses it today — add a case when needed).
- `inbound_handlers.go` — HTTP handlers: `handleAnthropicMessages`,
  `handleAnthropicCountTokens`, `handleChatCompletions` + SSE writers +
  Core-usage-based stats/logging (`recordInboundCompletion`).

### 3.4 Cross-cutting machinery (reused, not reimplemented)

- **Web search injection**: `injectCoreWebSearch` replaces `web_search`
  tools in `CoreRequest.Tools` with `tavily_search`/`firecrawl_fetch`
  (`internal/extension/websearchinjected`); execution loops:
  `executeChatSearchLoop`/`chatSearchBufferedStream` (wire-level, openai-chat)
  and `executeCoreSearchLoop` (Core-level, visual path) in
  `websearch_inject.go`. Resolution is **startup-only** — any web_search
  config change requires a container restart.
- **Visual orchestration** (`internal/extension/visual`): per-model config
  routes image inputs through a non-streaming Core loop with synthesized
  streams. `wrapWithVisual` returns nil unless the model has visual config.
- **DeepSeek reasoning replay**: reasoning is cached per session
  (`cacheReasoningForChat`) and prepended to follow-up requests
  (`prependCachedReasoningForChat`/`prependCachedThinking`).
- **Config graph**: SQLite (`data/moonbridge.db`, `config_store_*` tables) is
  the live source of truth, managed via `/api/v1/config/graph`
  (`internal/service/configgraph`); `config.yml` is the seed and mirror
  (and the input for codex catalog generation). Secrets are masked (`***`)
  in graph GETs — read real keys from `config.yml`.
- **Model resolution** (`internal/service/provider/manager.go`
  `ResolveModel`): route alias → `provider/model` or `model(provider)` ref →
  dynamic catalog. The `provider/model` slug ids exposed by `/v1/models`
  resolve natively.
- **Sessions** (`session.go`): keyed by `Session_id`, `X-Codex-Window-Id`,
  or `X-Claude-Code-Session-Id` headers; ephemeral otherwise.

### 3.5 House conventions

- Go 1.25+ module `moonbridge` (binary name still `moonbridge` — cosmetic).
- Package docs on every file; Chinese log/error messages in the server
  layer (`"请求完成"`, `"读取请求体失败"`); English code comments.
- `internal/protocol/*` must not import `internal/service` or
  `internal/extension` (no reverse dependencies).
- Test tiers: unit tests per package (`go test ./...`), `internal/e2e/`
  (build tag `e2e`, mock-upstream round trips), `internal/service/e2e/`
  (full HTTP). New adapters should get both conversion unit tests (see
  `client_adapter_test.go` files for the pattern: canned Core events →
  assert emitted wire chunks) and live end-to-end verification.
- Do not trust a change without a **live streaming request in the consumer's
  exact wire shape** (see the verification matrix below).

---

## 4. Deployment (homelab)

Lives on host "mini" at `/opt/docker/provider-bridge`:
`docker-compose.yml` (container `provider-bridge`, image `provider-bridge:latest`,
port 38440), `config.yml` (seed/mirror), `data/` (SQLite config graph),
`PATCHES.md` (deployment runbook incl. rollback), `update.sh`, `codex_regen.sh`.

Gotchas that bite (each caused a real incident):
- The container is **distroless nonroot**: `data/` must be owned
  `65532:65532`, `config.yml` must be `644` — otherwise startup fails with
  misleading config-error messages ("permission denied" buried in Chinese
  diagnostics).
- Passing `-config /config/config.yml` is required (the compose command does
  it; a bare `docker run` without it silently creates a default config at
  `$HOME/moonbridge/config.yml`).
- Copying `moonbridge.db` without its `-wal`/`-shm` siblings loses recent
  transactions when the source is running; stop the source container first
  (a clean close checkpoints the WAL — then only the main db is needed).

Consumers connect to `mini:38440`, or from the Mac through the SSH tunnel
LaunchAgent `com.fiorinis.moonbridge-tunnel` (`localhost:38440`).

### 4.1 End-to-end verification matrix (run after any meaningful change)

Against the live bridge (token = `server.auth_token` in `config.yml`):

1. **Codex shape**: `POST /v1/responses`, streaming, a function tool +
   reasoning → expect tool round-trip + `response.completed`.
2. **Claude Code shape**: `POST /v1/messages` streaming AND non-stream, with
   thinking and with tools → expect proper SSE sequence / content blocks;
   then `claude -p "Reply with exactly: OK" --model sonnet` (and haiku).
3. **LibreChat shape**: `POST /v1/chat/completions` non-stream + streaming
   with tools (arguments must be JSON strings) → verify from inside the
   LibreChat container against `host.docker.internal:38440`.
4. **Models**: `GET /v1/models` → OpenAI `object`/`data[]` with slug ids,
   deduplicated against route aliases.
5. **Web search**: a `web_search` tool request through `/v1/messages` or
   `/chat/completions` → the injected tavily search executes server-side
   (answer contains fresh facts, no `tool_use` blocks leak to the client).

---

## 5. Development workflow

The canonical dev clone is `~/Projects/provider-bridge/src` (Mac), remote
`origin` = `github.com/sfiorini/provider-bridge` (private). No local Go/Docker
on the Mac: build and test on mini inside `golang:1.26-bookworm`.

    # edit on the Mac, then:
    rsync -a --delete ~/Projects/provider-bridge/src/ mini:/tmp/pb-build/   # or to the deploy src
    ssh mini 'sudo docker run --rm -v pb-gomod:/go/pkg/mod -v /tmp/pb-build:/app -w /app golang:1.26-bookworm go test ./...'
    # deploy: rsync to /opt/docker/provider-bridge/src (sudo), docker compose build && up -d

**Commit before deploying** so GitHub, mini and the Mac stay in sync.
Deployments follow `PATCHES.md`; the update procedure for upstream changes is
`update.sh` (git pull + rebuild + health check).

Auto-synced provider: the OpenCode Zen free models are reconciled daily by
`~/.local/bin/moonbridge-zen-sync` on the Mac (LaunchAgent) — it talks to the
config graph via the tunnel, mirrors `config.yml`, restarts the container,
and regenerates + installs the Codex catalog. Don't hand-edit zen models.

## 6. Where this is going

Provider Bridge is its own product now. Direction:

- More inbound protocols where consumers need them (e.g. Gemini-native,
  OpenAI Realtime) — the ClientAdapter seam makes each a contained addition.
- Google-genai upstream support through the new executor (case exists in the
  original dispatch; port it when a Gemini provider is actually configured).
- Reconcile the two dispatch paths (fold `handleWithAdapters` into
  `executeCoreUpstream`) once the inbound surface is proven stable over
  time — until then, the Codex path stays untouched by policy.
- Polish for a potential public release: docs, example configs, and upstream
  contribution of the generic fixes (tool_use in synthesized streams).
- The upstream moon-bridge remains a sibling project: pull useful changes,
  push generic fixes back if they'll take them.
