# Changelog

All notable changes to this project will be documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.1.0/),
and this project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [Unreleased]

## [0.2.2] - 2026-10-08

### Changed

- Documentation: the multi-key `api_key` rotation (comma-separated keys,
  429/402 rotation, persisted active key) is now documented in
  `docs/CONFIGURATION.md`, `docs/COOKBOOK.md` (new recipe 9),
  `docs/API.md`, `docs/GETTING-STARTED.md`, `README.md`, and the web console's
  `api_key` field description.

## [0.2.1] - 2026-10-08

### Fixed

- Multi-turn chat conversations through consumers like LibreChat failed from the second turn on with a Mistral `422` (`reasoning_content` is not permitted on input messages). v0.2.0's issue-#11 fix made the bridge emit `reasoning_content` deltas, consumers replayed them in the next turn, and the chat provider adapter echoed them to every provider unconditionally. The upstream echo is now gated on the provider (DeepSeek only, matching the existing reasoning-replay precedent); consumer-side reasoning display is unchanged. Regression tests cover the exact replayed-conversation shape.

## [0.2.0] - 2026-10-08

### Added

- Multi-key API-key rotation: a provider's `api_key` may now contain multiple keys separated by commas. On HTTP 429 (rate limited) or 402 (quota exhausted) the bridge retries the same request with the next key; the advanced key becomes the active key and is persisted across restarts in a dedicated `key_rotation` SQLite table. Rotation covers the Core upstream executor, the Responses-inbound adapter path, the web-search loops (same-round retry), and the raw OpenAI Responses passthrough (transport errors never rotate; the active-key response is replayed verbatim on full rotation failure). Google-genai rotation is excluded by design. Single-key providers behave exactly as before.
- Rotation machinery and tests: `config.SplitAPIKeys`, typed `chat.ProviderError` (error text unchanged), `provider.ChatCaller` with rotating chat/anthropic clients, per-path rotation tests, persistence/restart + reload-clamp + PATCH-`******`-secret-preservation tests, masking round-trip tests (graph GET, YAML export, LoadAll), e2e rotation tests (chat streaming, anthropic, persists-across-restart), and multi-key documentation in `config.example.yml`.

### Fixed

- GitHub issue #11: the chat upstream stream parser no longer drops Mistral array-form `delta.content` (`zai-glm-5-3` streamed empty). `Delta.UnmarshalJSON` tolerates string/array/null content and extracts thinking text; the non-stream path maps `thinking` parts to Core reasoning blocks. Regression tests at the unmarshaler, SSE-reader, stream-adapter, and e2e (mock upstream) levels.
- Boot-time chat clients are built from the provider's active key, so a comma-separated key list can no longer leak into an `Authorization` header via the visual-orchestration path or the `activeChatClient` fallback.
- Pre-existing e2e failure `TestPluginHooks_MultiplePlugins` (plugin 2 was created disabled, so its mutator never ran).

### Notes

- `TestGoogleGenaiE2E_ToolUseRoundTrip` fails on `main` (pre-existing, unrelated to this release) — tracked as issue #12.

## [0.1.0] - 2026-10-07

### Added

- Multi-protocol inbound gateway: OpenAI Responses, Anthropic Messages, and OpenAI Chat Completions, over a shared protocol-agnostic Core and one upstream executor.
- Server-side web-search injection (Tavily) and image/visual orchestration.
- Release & version tooling: `VERSION`, a `-version` CLI flag, `scripts/release.sh`, and CI-published Docker images on GHCR plus GitHub Releases.
- Public-release-ready documentation (deployment, consumers, API, testing, release).

### Changed

- Rebranded `moonbridge` -> `providerbridge` across the module, binary, runtime defaults, packaging, and docs.
- English-only documentation and Go log/error strings.
