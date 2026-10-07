# Changelog

All notable changes to this project will be documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.1.0/),
and this project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [Unreleased]

## [0.1.0] - 2026-10-07

### Added

- Multi-protocol inbound gateway: OpenAI Responses, Anthropic Messages, and OpenAI Chat Completions, over a shared protocol-agnostic Core and one upstream executor.
- Server-side web-search injection (Tavily) and image/visual orchestration.
- Release & version tooling: `VERSION`, a `-version` CLI flag, `scripts/release.sh`, and CI-published Docker images on GHCR plus GitHub Releases.
- Public-release-ready documentation (deployment, consumers, API, testing, release).

### Changed

- Rebranded `moonbridge` -> `providerbridge` across the module, binary, runtime defaults, packaging, and docs.
- English-only documentation and Go log/error strings.
