# Consumers

Provider Bridge serves every AI consumer in the homelab through one endpoint
and one token. Each consumer keeps speaking its native wire protocol; the
bridge converts through Core to the resolved upstream provider.

This page documents the reference deployment (the "mini" host). Hostnames and
model routes are deployment-specific — treat them as the reference and check
the live config graph (`GET /api/v1/routes`, `GET /api/v1/models`) for the
actual values.

## Connection targets

| From | Base URL |
|------|----------|
| Homelab host / LAN | `http://mini:38440` |
| Mac, through the SSH tunnel LaunchAgent | `http://localhost:38440` |
| Another Docker container on the same compose network | `http://provider-bridge:38440` |
| A container on a different network, reaching the host | `http://host.docker.internal:38440` |

Generic form: `http(s)://<bridge-host>:<port>` with the `Authorization: Bearer
<server.auth_token>` header whenever `server.auth_token` is configured.

## Consumer matrix

| Consumer | Inbound protocol | Endpoint | Connection target | Notes |
|----------|------------------|----------|-------------------|-------|
| Codex (OpenAI) | Responses | `POST /v1/responses` | `mini:38440/v1` | Default consumer; raw passthrough for Responses upstreams |
| Claude Code | Anthropic Messages | `POST /v1/messages` | `mini:38440` | Full SSE, thinking + tools; session header `X-Claude-Code-Session-Id` |
| LibreChat | Chat Completions | `POST /v1/chat/completions` | `host.docker.internal:38440` | Single "Provider Bridge" endpoint; model list fetched live |
| Open WebUI | OpenAI type, `api_type: responses` | `POST /v1/responses` (+ `GET /v1/models`) | `host.docker.internal:38440` | Reads the OpenAI shape of `/v1/models` |
| Affiora (morphic fork) | Chat Completions | `POST /v1/chat/completions` | `http://provider-bridge:38440` | Joined via the compose network `provider-bridge_default` |

## Codex

Codex speaks the OpenAI Responses API. Point its provider entry at the bridge;
any non-empty API key works when auth is enabled.

`~/.codex/config.toml`:

```toml
model = "gpt-5.4"
model_provider = "provider-bridge"

[model_providers.provider-bridge]
name = "Provider Bridge"
base_url = "http://mini:38440/v1"
api_key = "any-non-empty-value"
wire_api = "responses"
```

The bridge accepts `POST /v1/responses` (and the `/responses` alias), handles
function tools and reasoning, and returns `response.completed`. The Codex model
catalog can be regenerated from the bridge with the deployment helper
(`deploy/mini/codex_regen.sh`).

## Claude Code

Claude Code speaks the Anthropic Messages API. Set the base URL and token via
environment variables:

```bash
export ANTHROPIC_BASE_URL="http://mini:38440"
export ANTHROPIC_AUTH_TOKEN="<server.auth_token>"
```

Claude Code maps its model tiers to the bridge route aliases. In the reference
deployment:

| Claude tier | Route alias | Resolves to |
|-------------|-------------|-------------|
| `opus` | (opus alias) | `mistral/zai-glm-5-3` |
| `sonnet` | (sonnet alias) | `deepseek/deepseek-v4-pro` |
| `haiku` | (haiku alias) | `zen/space-bunny-free` |

`POST /v1/messages` supports streaming and non-streaming, thinking blocks with
signatures, tool_use round-trips, and `POST /v1/messages/count_tokens`.
Claude Code sends `X-Claude-Code-Session-Id`, which the bridge uses to key the
session and preserve reasoning replay across turns. A quick check:

```bash
claude -p "Reply with exactly: OK" --model sonnet
```

## LibreChat

LibreChat speaks Chat Completions. Configure a single "Provider Bridge"
endpoint and let LibreChat fetch the model list live.

`librechat.yaml` (custom endpoint):

```yaml
version: 1.2.8
endpoints:
  custom:
    - name: "Provider Bridge"
      apiKey: "<server.auth_token>"
      baseURL: "http://host.docker.internal:38440/v1"
      models:
        default: ["mistral/zai-glm-5-3"]
        fetch: true
      titleConvo: true
      modelDisplayLabel: "Provider Bridge"
```

`fetch: true` reads `GET /v1/models` (OpenAI shape) so the picker lists every
model without manual maintenance. Tool-call arguments arrive as JSON strings,
which LibreChat parses correctly.

## Open WebUI

Open WebUI is registered as an OpenAI-type connection with
`api_type: responses`, so it uses the Responses API and reads the OpenAI shape
of `/v1/models`.

Settings → Connections → add an OpenAI connection:

| Field | Value |
|-------|-------|
| Base URL | `http://host.docker.internal:38440/v1` |
| API key | `<server.auth_token>` |
| API type | `responses` |

The `/v1/models` response carries `{"object":"list","data":[...]}` with
`provider/model` slug ids, deduplicated against route aliases, so the model
picker shows each model once.

## Affiora (morphic fork)

Affiora speaks Chat Completions and joins the bridge over the compose network
`provider-bridge_default`, so it uses the container name directly.

Relevant environment variables:

```bash
OPENAI_API_KEY="<server.auth_token>"
OPENAI_API_BASE="http://provider-bridge:38440/v1"
OPENAI_COMPATIBLE_API_BASE="http://provider-bridge:38440/v1"
DEFAULT_MODEL="mistral/zai-glm-5-3"
```

Because Affiora is attached to `provider-bridge_default`, the DNS name
`provider-bridge` resolves to the bridge container; no published port or host
gateway is required. The AI SDK data-stream path is Chat Completions with
streaming tool calls.

## Adding a consumer

1. Pick the wire protocol the consumer already speaks (Responses, Anthropic
   Messages, or Chat Completions).
2. Point its base URL at the bridge and set the bearer token.
3. Give it a model id that resolves: a route alias, or a `provider/model` slug
   from `GET /v1/models`.
4. If the consumer cannot execute tools you want it to use (for example
   server-side web search), configure web search — see
   [WEB-SEARCH.md](WEB-SEARCH.md).

## Related

- [API.md](API.md) — full endpoint and streaming reference.
- [ARCHITECTURE.md](ARCHITECTURE.md#request-paths) — the three inbound paths.
- [GETTING-STARTED.md](GETTING-STARTED.md) — first request from scratch.
