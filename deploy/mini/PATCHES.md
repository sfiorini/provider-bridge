# Provider Bridge — deployment runbook

Provider Bridge is Stefano's private fork of moon-bridge
(https://github.com/ZhiYi-R/moon-bridge), living at
https://github.com/sfiorini/provider-bridge (private). The fork carries
the upstream history plus merged local commits — there is no rebase step
anymore; all changes are committed on `main`.

## What the fork adds over upstream

| Commit area | What it does |
|---|---|
| openai-chat + Codex interop (5 commits) | reasoning_content replay gating, web_search tool stripping, web_search support config, Core search loop for the visual path, OpenAI-compatible `data[]` in `/v1/models` |
| inbound Anthropic Messages | `POST /v1/messages` (+ `/v1/messages/count_tokens`) — full Claude Code-grade streaming, thinking + tool_use blocks, polymorphic wire normalization |
| inbound Chat Completions | `POST /v1/chat/completions` — streaming chunks, reasoning_content, tool_calls, include_usage, legacy max_tokens |
| shared Core upstream executor | both new inbounds reuse provider adapters, web-search injection, visual orchestration, DeepSeek reasoning replay, sessions (incl. X-Claude-Code-Session-Id), tracing, usage stats; the OpenAI Responses dispatch is untouched |
| upstream fixes | the visual-path stream synthesizers dropped tool_use blocks (all inbound protocols lost tool calls on that path) |

## Consumers

| Consumer | Wire | Notes |
|---|---|---|
| Codex (Mac) | `/v1/responses` via localhost:38440 tunnel | unchanged |
| Open WebUI (mini) | OpenAI type, api_type `responses` | unchanged |
| LibreChat (mini) | one endpoint "Provider Bridge", baseURL `http://host.docker.internal:38440/v1`, key `${BRIDGE_API_KEY}` (in `.env`), models fetched live | Google/Gemini stays direct |
| Claude Code (Mac) | Anthropic Messages via `http://localhost:38440`, token in `~/.claude/settings.json`; opus/sonnet=`deepseek/deepseek-v4-pro`, haiku=`zen/space-bunny-free` (free) | |

## Updating

    sudo /opt/docker/provider-bridge/update.sh

Pulls origin/main, rebuilds, restarts, health-checks. On merge conflicts
(rare — it's our own repo): `cd src && git status`, resolve,
`git pull --ff-only` again.

To develop: edit on the Mac in `~/Projects/provider-bridge/src` (repo,
remote origin = GitHub), then:

    rsync -a --delete ~/Projects/provider-bridge/src/ mini:/opt/docker/provider-bridge/src/
    ssh mini 'cd /opt/docker/provider-bridge && sudo docker compose build && sudo docker compose up -d'

Commit before deploying so mini and GitHub stay in sync.

## Codex catalog regeneration

    sudo /opt/docker/provider-bridge/codex_regen.sh

Writes `generated_configs/`; copy `models_catalog.json` to the Mac at
`~/.codex/models_catalog.json` if it changed (the zen-sync script does
this automatically on its daily run).

## OpenCode Zen free models (auto-synced)

`~/.local/bin/moonbridge-zen-sync` on the Mac (LaunchAgent
`com.fiorinis.moonbridge-zen-sync`, daily 10:00) reconciles the zen
provider, mirrors `config.yml`, restarts the **provider-bridge**
container, and regenerates + installs the Codex catalog. The key lives
at `~/.config/opencode-zen.key`.

## Architecture notes

- Live config = SQLite config graph (`data/moonbridge.db`, tables
  `config_store_*`); `config.yml` is the seed/mirror. Edit the live
  config through the management API (`/api/v1/config/graph`), then sync
  `config.yml` to match (codex_regen.sh reads it).
- Web-search resolution is startup-only: every web_search config change
  needs `docker restart provider-bridge`. Verify the boot log line
  `配置启用网页搜索注入模式 provider=<name>`.
- `data/` must be owned by 65532:65532 and `config.yml` readable by
  nonroot (644) — the container is distroless/nonroot.
- Mac access: `ssh mini`; codex/Claude Code reach the bridge through the
  `com.fiorinis.moonbridge-tunnel` LaunchAgent (localhost:38440).
- Management token in `config.yml` (`server.auth_token`).

## Rollback

The old moon-bridge deployment is kept (stopped) at
`/opt/docker/moon-bridge` with its image `moonbridge:latest`. To roll
back: `docker compose down` in provider-bridge, `docker compose up -d`
in moon-bridge. Consumers that need reverting: LibreChat endpoint
(`librechat.yaml.bak.pre-bridge`, `.env.bak.pre-bridge` — recreate api
container) and Claude Code (`~/.claude/settings.json`, restore
DeepSeek-direct values).
