# Getting Started

> Get a first conversation working in about five minutes. More recipes live in
> [COOKBOOK.md](COOKBOOK.md).

## 1. Install

### Requirements

- **Go 1.25+** — to build and run.
- An API key for at least one upstream provider (DeepSeek, OpenAI, Anthropic,
  Kimi, …).

### Get the code

This is the Provider Bridge fork:

```bash
git clone git@github.com:sfiorini/provider-bridge.git
cd provider-bridge
```

The Go module is named `providerbridge`.

### Build

```bash
go build -o providerbridge ./cmd/providerbridge
```

You can also run straight from source:

```bash
go run ./cmd/providerbridge
```

## 2. Configure

If `-config` is omitted and `$HOME/provider-bridge/config.yml` does not exist,
Provider Bridge creates a starter config with SQLite enabled and the database at
`$HOME/provider-bridge/data/provider-bridge.db`, then starts. Open the Web
Console:

```text
http://127.0.0.1:38440/console/
```

Before sending real requests, replace the placeholder provider, model and API
key in the starter config — either in the Console or by editing the YAML.

To maintain the YAML directly, copy [`../config.example.yml`](../config.example.yml)
and adjust it. See [CONFIGURATION.md](CONFIGURATION.md) for the full reference.

### Minimal config (DeepSeek example)

```yaml
mode: "Transform"

server:
  addr: "127.0.0.1:38440"

defaults:
  model: "deepseek-v4-pro"

models:
  deepseek-v4-pro:
    context_window: 1000000
    max_output_tokens: 384000
    input_modalities: ["text"]

providers:
  deepseek:
    protocol: "anthropic"
    base_url: "https://api.deepseek.com/anthropic"
    api_key: "sk-your-api-key"
    version: "2023-06-01"
    offers:
      - model: deepseek-v4-pro

routes:
  default:
    model: deepseek-v4-pro
    provider: deepseek
```

### Supported upstream protocols

| Protocol | `protocol` value | Example providers |
|----------|------------------|-------------------|
| Anthropic Messages | `anthropic` | DeepSeek, Kimi, Anthropic |
| OpenAI Responses | `openai-response` | OpenAI (passthrough) |
| Google GenAI (Gemini) | `google-genai` | Google Gemini |
| OpenAI Chat | `openai-chat` | OpenAI-chat-compatible APIs |

## 3. Start

```bash
providerbridge -config config.yml
```

Startup output looks like this (English since the fork's English-only rewrite):

```text
Provider Bridge listening on 127.0.0.1:38440
Web Console: http://127.0.0.1:38440/console/
time=2026-10-06T12:00:00.000+02:00 level=INFO msg="config loaded" path=config.yml mode=Transform addr=127.0.0.1:38440
time=2026-10-06T12:00:00.000+02:00 level=INFO msg="HTTP server listening" addr=127.0.0.1:38440 webui=http://127.0.0.1:38440/console/
```

## 4. Verify the model list

```bash
curl http://127.0.0.1:38440/v1/models
```

The OpenAI-shaped response is `{"object":"list","data":[...]}` with
`provider/model` slug ids; the legacy `{"models":[...]}` shape is also present.

## 5. First chat

Chat Completions:

```bash
curl http://127.0.0.1:38440/v1/chat/completions \
  -H "Content-Type: application/json" \
  -H "Authorization: Bearer any-value" \
  -d '{"model": "default", "messages": [{"role": "user", "content": "Hello"}]}'
```

Responses:

```bash
curl http://127.0.0.1:38440/v1/responses \
  -H "Content-Type: application/json" \
  -H "Authorization: Bearer any-value" \
  -d '{"model": "default", "input": "Hello"}'
```

Anthropic Messages:

```bash
curl http://127.0.0.1:38440/v1/messages \
  -H "Content-Type: application/json" \
  -H "x-api-key: any-value" \
  -d '{"model": "default", "max_tokens": 256, "messages": [{"role": "user", "content": "Hello"}]}'
```

If `server.auth_token` is set, send that value as the Bearer token (or
`x-api-key`) instead of `any-value`.

## Next steps

- [CONSUMERS.md](CONSUMERS.md) — wiring Codex, Claude Code, LibreChat, Open
  WebUI and Affiora to the bridge.
- [COOKBOOK.md](COOKBOOK.md) — common task recipes (macOS/Linux and Windows).
- [CONFIGURATION.md](CONFIGURATION.md) — complete configuration reference.
- [ARCHITECTURE.md](ARCHITECTURE.md) — how the bridge is put together.
