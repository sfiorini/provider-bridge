# Provider Bridge

**Provider Bridge is a self-hosted, multi-protocol AI model gateway.** One
process, one config, and one token put every AI consumer in your home network in
front of every model provider. Consumers keep speaking their **native wire
protocol** — the OpenAI Responses API, the Anthropic Messages API, or OpenAI
Chat Completions — and the bridge translates all of them through a single
internal representation ("Core") to any upstream provider protocol. Change a
provider (add or rotate keys, swap models, adjust pricing) in one place and every
consumer sees the change.

## Features

- **Inbound protocols** — OpenAI Responses (`/v1/responses`), Anthropic Messages
  (`/v1/messages`, plus `/v1/messages/count_tokens`), OpenAI Chat Completions
  (`/v1/chat/completions`), and model listing (`/v1/models`).
- **Server-side web-search injection** — the bridge substitutes a `web_search`
  tool with `tavily_search` / `firecrawl_fetch` and executes the calls itself,
  invisible to the consumer.
- **Capability-driven visual orchestration** — image inputs to text-only models
  are routed through a vision-capable model and synthesized back into the
  consumer's stream.
- **Per-model pricing, usage stats, and tracing** — token and cost accounting
  per session/model, with full request/response traces.
- **Embedded web console** — configure providers, models, routes, and keys at
  `/console/`.
- **SQLite config graph + management API** — the live config graph is the source
  of truth, managed through `/api/v1/` (`config.yml` is the seed and mirror).
- **Model aliases and routes** — map a consumer-facing alias to any
  `provider/model` upstream, resolved at request time.

## Consumers

Each consumer keeps its native wire protocol; only the base URL (and token)
change. Full per-app setup lives in [docs/CONSUMERS.md](docs/CONSUMERS.md).

| Consumer | Wire protocol | Connection target |
|----------|---------------|-------------------|
| Codex (OpenAI) | Responses (`/v1/responses`) | `https://my-provider-bridge:38440/v1` |
| Claude Code | Anthropic Messages (`/v1/messages`) | `https://my-provider-bridge:38440` |
| LibreChat | Chat Completions (`/v1/chat/completions`) | `https://my-provider-bridge:38440/v1` |
| Open WebUI | OpenAI type, `api_type: responses` | `https://my-provider-bridge:38440/v1` |
| Affiora | Chat Completions (`/v1/chat/completions`) | `http://providerbridge:38440/v1` |

Generic form: `http(s)://<bridge-host>:<port>` with the
`Authorization: Bearer <server.auth_token>` header when `server.auth_token` is
configured.

## Endpoints

| Endpoint | Method | Description |
|----------|--------|-------------|
| `/v1/responses` | POST | OpenAI Responses API inbound (alias: `/responses`) |
| `/v1/messages` | POST | Anthropic Messages API inbound |
| `/v1/messages/count_tokens` | POST | Anthropic token counting |
| `/v1/chat/completions` | POST | OpenAI Chat Completions inbound |
| `/v1/models` | GET | Model list; OpenAI `{"object":"list","data":[…]}` plus the legacy shape (alias: `/models`) |
| `/console/` | GET | Embedded web console |
| `/api/v1/` | — | Management API (providers, models, routes, config graph, stats, logs) |

The full HTTP reference — streaming shapes, error envelopes, sessions, and the
management API — is in [docs/API.md](docs/API.md).

## Quick start

```bash
# Clone and build
git clone git@github.com:sfiorini/provider-bridge.git
cd provider-bridge
go build -o providerbridge ./cmd/providerbridge

# Start with an explicit config (-config is required in a container)
./providerbridge -config config.yml
# First run without a config creates $HOME/provider-bridge/config.yml and starts.

# Send your first request
curl http://127.0.0.1:38440/v1/chat/completions \
  -H "Content-Type: application/json" \
  -H "Authorization: Bearer any-value" \
  -d '{"model": "default", "messages": [{"role": "user", "content": "Hello"}]}'
```

Replace the placeholder provider, model, and API key before sending real
requests — either in the console at `http://127.0.0.1:38440/console/` or in the
YAML. Go 1.25+ is required to build.

See [docs/GETTING-STARTED.md](docs/GETTING-STARTED.md) for the five-minute
onboarding walkthrough.

## Documentation

The full documentation index is in [docs/README.md](docs/README.md):

- [Getting Started](docs/GETTING-STARTED.md) — build, configure, first request
- [Cookbook](docs/COOKBOOK.md) — task-oriented recipes
- [Configuration](docs/CONFIGURATION.md) — complete YAML reference
- [API](docs/API.md) — HTTP endpoints, streaming, management API
- [Architecture](docs/ARCHITECTURE.md) — Core IR, adapters, request paths
- [Consumers](docs/CONSUMERS.md) — Codex, Claude Code, LibreChat, Open WebUI, Affiora
- [Web Search](docs/WEB-SEARCH.md) — server-side search injection
- [Testing](docs/TESTING.md) and [Development](docs/DEVELOPMENT.md)
- [Deployment](docs/DEPLOYMENT.md) — binary, systemd, Docker, Cloudflare

## License

[GPL v3](LICENSE)

---

*Provider Bridge is a diverging fork of [moon-bridge](https://github.com/ZhiYi-R/moon-bridge) by ZhiYi-R — many thanks to the original developers for the foundation this project was built on.*
