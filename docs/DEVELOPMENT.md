# Development

## Prerequisites

- **Go 1.25+** (the module declares `go 1.25.0`; CI and the mini build use
  `golang:1.27-bookworm`).
- **Node + npm** for the Web Console (`webui/`).
- Optional: an upstream provider API key for live E2E runs.
- The reference build/test host has no local Go — commands run inside a
  `golang` container (see below).

## Project structure

```
cmd/
  providerbridge/          # main binary entry point
  cloudflare/              # Cloudflare Worker (WASM) entry point

internal/
  config/                  # YAML loading, validation, JSON schema
  db/                      # database abstraction + provider registry
  e2e/                     # protocol conversion E2E tests (//go:build e2e)
  extension/               # pluggable extensions
    codex/                 # Codex model catalog / config.toml generation
    codex_tool_proxy/      # apply_patch proxy (structured sub-tools)
    codextool/             # Codex tool types + mapping
    db/                    # db providers: sqlite/, d1/
    deepseek_v4/           # DeepSeek V4 reasoning handling
    kimi_workaround/       # Kimi tool-call round limiter
    metrics/               # usage metrics
    plugin/                # Plugin interface, capabilities, registry
    visual/                # visual orchestration (CoreProvider)
    websearch/             # web-search orchestrator
    websearchinjected/     # injected web-search module
  format/                  # Core types, Adapter interfaces, Registry
  logger/                  # slog wrapper + log buffering
  modelref/                # model reference parsing/resolution
  openai_dto/              # shared OpenAI DTO types
  protocol/                # protocol conversions
    anthropic/             # Anthropic Messages adapter (in + upstream)
    cache/                 # prompt cache planning
    chat/                  # OpenAI Chat adapter (in + upstream)
    google/                # Google Gemini (GenAI) adapter
    openai/                # OpenAI Responses adapter
  service/                 # business orchestration
    api/                   # management REST API (routes in router.go)
    app/                   # application lifecycle + extension catalog
    configgraph/           # SQLite config graph + patch/validation
    e2e/                   # service-level HTTP E2E tests
    provider/              # provider manager / model resolution
    proxy/                 # Capture-mode proxying
    runtime/               # runtime context / hot reload
    server/                # HTTP server, routes, auth, dispatch
    stats/                 # usage statistics
    store/                 # config persistence
    trace/                 # request tracing
    webui/                 # embedded Web Console (go:embed) + dist/
  session/                 # session management
```

`internal/protocol/format` is a legacy leftover: it still exists on disk but is
no longer imported anywhere. New code must use `internal/format`.

## Build

```bash
go build -o providerbridge ./cmd/providerbridge      # main binary
go build -o worker.wasm ./cmd/cloudflare             # Cloudflare Worker
make build                                           # build all packages
```

## Test on the mini build host

The reference workflow builds and tests on `mini` inside a Go container. From
the repo root on the Mac:

```bash
rsync -a --delete ./src/ mini:/tmp/pb-build/     # or the deploy src
ssh mini 'sudo docker run --rm \
  -v pb-gomod:/go/pkg/mod \
  -v /tmp/pb-build:/app -w /app \
  golang:1.27-bookworm go test ./...'
```

The `-tags=e2e` build tag enables the protocol E2E suite:

```bash
ssh mini 'sudo docker run --rm \
  -v pb-gomod:/go/pkg/mod \
  -v /tmp/pb-build:/app -w /app \
  golang:1.27-bookworm go test -tags=e2e ./internal/e2e/... ./internal/service/e2e/...'
```

Common local commands:

```bash
go test ./...                                   # unit tests
go test ./internal/protocol/anthropic/...       # package-level
go test ./internal/e2e/... -v -count=1          # requires -tags=e2e for real runs
make cover-check                                # enforced per-package coverage
```

See [TESTING.md](TESTING.md) for the four test tiers and the live verification
matrix.

## Web Console development

The Console is a Vite/React frontend embedded into the Go binary and served at
`/console/`.

```bash
npm --prefix webui install
npm --prefix webui run dev        # http://127.0.0.1:5173/console/

npm --prefix webui test           # vitest unit tests
npm --prefix webui run e2e        # e2e tests
npm --prefix webui run build      # production build

make webui-build                  # build + copy into internal/service/webui/dist
make build-with-webui             # Go build with the embedded Console
```

The dev server proxies `/api`, `/v1`, `/responses` and `/models` to
`127.0.0.1:38440`. Running the preview backend needs a config with
`persistence.active_provider` set, otherwise the `/api/v1/` management API is
not registered and the Console shows a setup/unavailable state.

The Console config pages use the config-graph API:

- `GET /api/v1/config/graph`
- `PATCH /api/v1/config/graph`
- `POST /api/v1/config/graph/validate`
- `POST /api/v1/config/resources/{kind}`
- `DELETE /api/v1/config/resources/{kind}/{id}`
- `GET /api/v1/logs/recent`
- `GET /api/v1/logs/stream`

Relevant test commands:

```bash
npm --prefix webui test -- configGraph logs
env GOCACHE=/tmp/providerbridge-go-build GOMODCACHE=/tmp/providerbridge-go-mod \
  go test ./internal/service/api ./internal/service/webui ./internal/service/server
```

`webui/dist/` is not committed; `make webui-build` copies the build into
`internal/service/webui/dist/`, which is what `go:embed` packages.

## Run

```bash
go run ./cmd/providerbridge -config config.yml
```

Config changes are applied via the management API or a restart (see
[CONFIGURATION.md](CONFIGURATION.md) for hot-reload vs restart-required fields).

## Adding a Provider Adapter (upstream)

An upstream adapter converts Core requests to a provider wire format and back.

1. Add the protocol constant in `internal/config/config.go`
   (`ProtocolAnthropic`, `ProtocolOpenAIChat`, …).
2. Create `internal/protocol/<adapter>/` and implement
   `format.ProviderAdapter` and `format.ProviderStreamAdapter` from
   `internal/format/adapter.go`.
3. Register the adapter in `internal/service/app/app.go` with
   `adapterReg.RegisterProvider(...)` and
   `adapterReg.RegisterProviderStream(...)`.
4. Add a protocol branch to the dispatch layer
   (`internal/service/server/adapter_dispatch.go` for the Responses path and
   `internal/service/server/core_upstream.go` for the Core path).
5. Add conversion unit tests following the `*_adapter_test.go` pattern
   (canned Core events → assert emitted wire chunks) plus an E2E case under
   `internal/e2e/`.

## Adding an inbound ClientAdapter

An inbound adapter converts a consumer's native wire format into Core and the
Core stream back out.

1. Implement `format.ClientAdapter` and `format.ClientStreamAdapter`
   (`ToCoreRequest` / `FromCoreResponse` / `FromCoreStream`).
2. Register it in `app.go` with `adapterReg.RegisterClient(...)` and
   `adapterReg.RegisterClientStream(...)`.
3. Add an HTTP handler in `internal/service/server/inbound_handlers.go` and
   register the route in `server.go`, converting to Core early and handing off
   to `executeCoreUpstream`.
4. Add conversion unit tests (canned Core stream → assert wire chunks) and a
   live wire-shape test in the consumer's exact shape.

## Management API development

Management endpoints live in `internal/service/api/`; routes are created by
`NewRouter` (`router.go`). Handlers operate on the config graph
(`internal/service/configgraph`) or the runtime services, and are exercised by
`internal/service/api/*_test.go`.

## Code conventions

See [DEVELOPMENT-CONVENTIONS.md](DEVELOPMENT-CONVENTIONS.md). Highlights:
error and log messages are English; file names describe their responsibility;
`internal/format` is the single intermediate representation.

## Contributing

Branch policy, the contribution flow and the adapter walkthroughs are covered in
[../CONTRIBUTING.md](../CONTRIBUTING.md); keep the "adding a provider/adapter"
steps there and here in sync.
