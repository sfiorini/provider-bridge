# Provider Bridge Documentation

Provider Bridge is a self-hosted, multi-protocol AI model gateway. One process,
one config, and one token put every AI consumer in the homelab in front of every
model provider. Consumers keep speaking their native wire protocol — OpenAI
Responses, Anthropic Messages, or Chat Completions — and the bridge translates
all of them through one internal representation ("Core") to any upstream
provider protocol.

The tables below index every document in this directory. Deeper topics that are
still being written are marked **planned**.

## Start here

| Document | What it covers |
|----------|----------------|
| [GETTING-STARTED.md](GETTING-STARTED.md) | Five-minute onboarding: build, starter config, first request, next steps |
| [COOKBOOK.md](COOKBOOK.md) | Task-oriented recipes (macOS/Linux and Windows command pairs): first chat, Codex CLI, provider swap, reasoning, images, web search, cache, troubleshooting |
| [CONFIGURATION.md](CONFIGURATION.md) | Full YAML reference: modes, server, models, providers, routes, web search, cache, extensions, proxy, CLI flags |
| [CONFIG-MIGRATION.md](CONFIG-MIGRATION.md) | Migrating a legacy (v4) config to the current v5 shape |

## Core reference

| Document | What it covers |
|----------|----------------|
| [API.md](API.md) | HTTP API reference: inbound endpoints, streaming shapes, `/v1/models`, management API, config-graph API, errors, sessions |
| [ARCHITECTURE.md](ARCHITECTURE.md) | Four-layer architecture, the three inbound request paths, Core IR, adapter registry, cross-cutting machinery |
| [EXTENSION-SYSTEM.md](EXTENSION-SYSTEM.md) | Plugin interfaces, capability types, the registry, server/persistence integration, lifecycle |
| [EXTENSIONS.md](EXTENSIONS.md) | Catalogue of shipped extensions: deepseek_v4, visual, web search injection, kimi_workaround, codex, databases, metrics |
| [WEB-SEARCH.md](WEB-SEARCH.md) | Server-side web-search injection: config scopes, support modes, execution loops, startup-only resolution |
| [CONSUMERS.md](CONSUMERS.md) | Consumer matrix: Codex, Claude Code, LibreChat, Open WebUI, Affiora — wire protocol, connection target, per-app config |

## Development

| Document | What it covers |
|----------|----------------|
| [DEVELOPMENT.md](DEVELOPMENT.md) | Dev setup, project tree, build, Web Console development, adding a provider or inbound adapter |
| [DEVELOPMENT-CONVENTIONS.md](DEVELOPMENT-CONVENTIONS.md) | Package layout, dependency direction, naming, error handling, logging, config evolution, testing rules |
| [TESTING.md](TESTING.md) | Test tiers (unit, protocol e2e, service e2e, management API) and the live wire-shape verification matrix |
| [webui/MATERIAL-COMPONENT-DEBT.md](webui/MATERIAL-COMPONENT-DEBT.md) | Web UI Material-Web migration backlog and review requirements |

## Deployment

| Document | What it covers |
|----------|----------------|
| [DEPLOYMENT.md](DEPLOYMENT.md) | Binary, systemd, nginx, Docker/compose, Cloudflare Workers, config management, homelab gotchas |

### Deployment runbooks

| Document | What it covers |
|----------|----------------|
| [deploy/mini/PATCHES.md](../deploy/mini/PATCHES.md) | Deployment runbook for the mini host, including rollback |
| [deploy/mini/update.sh](../deploy/mini/update.sh) | Pull the repo and redeploy the mini container |
| [deploy/mini/codex_regen.sh](../deploy/mini/codex_regen.sh) | Regenerate Codex `config.toml` and `models_catalog.json` from the bridge |
| [deploy/mini/MODEL-METADATA-RUNBOOK.md](../deploy/mini/MODEL-METADATA-RUNBOOK.md) | Reconcile live model metadata with the verified `INVENTORY.md` values |
| [deploy/mini/RENAME-CUTOVER.md](../deploy/mini/RENAME-CUTOVER.md) | Runtime rename cutover runbook (legacy name → `providerbridge`) |

## Related

- [Model inventory](../INVENTORY.md) — every deployed model slug with verified
  context window, max output tokens, input modalities, and verification status.
- [Contributing](../CONTRIBUTING.md) — contribution flow and branch policy.
