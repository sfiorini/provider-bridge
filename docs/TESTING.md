# Testing

Provider Bridge uses the Go standard library `testing` package — no external
test framework. Protocol E2E tests are gated behind the `e2e` build tag.

## Running tests

```bash
# All unit tests
go test ./...

# Package-level
go test ./internal/protocol/anthropic/...

# Verbose
go test -v -count=1 ./internal/protocol/...

# Coverage
go test -coverprofile=coverage.out ./...
go tool cover -html=coverage.out -o coverage.html
```

On a workstation without a local Go toolchain, run the suites inside the
`golang:1.27-bookworm` container (either locally or on your build host):

```bash
docker run --rm \
  -v "$PWD":/app -w /app \
  golang:1.27-bookworm go test ./...

docker run --rm \
  -v "$PWD":/app -w /app \
  golang:1.27-bookworm go test -tags=e2e ./internal/e2e/... ./internal/service/e2e/...'
```

`make test` runs `go test ./...`; `make cover-check` enforces the per-package
coverage floor.

## The four test tiers

### 1. Unit tests

Co-located with the package under test. Covers adapter conversions, server
routing/handling, extensions and foundation packages. External HTTP is mocked;
no live API keys required.

### 2. Protocol E2E tests (`internal/e2e/`, `//go:build e2e`)

Full request/response conversion against mock upstreams. Files:

| File | Coverage |
|------|----------|
| `anthropic_e2e_test.go` | Anthropic Messages conversion |
| `google_genai_e2e_test.go` | Google Gemini conversion |
| `openai_chat_e2e_test.go` | OpenAI Chat conversion |
| `openai_response_e2e_test.go` | OpenAI Responses passthrough |
| `plugin_hooks_e2e_test.go` | `CorePluginHooks` end-to-end |
| `websearch_injection_e2e_test.go` | Web-search injection path |
| `visual_chat_e2e_test.go` | Visual orchestration over the Chat path |
| `e2e_test.go` | Shared harness (mock upstreams, SSE helpers, `TestMain`) |

Run them in mock mode with no keys:

```bash
go test -tags=e2e ./internal/e2e/... -v -count=1
```

For a real-provider run, copy `.env.test.example` to `.env.test` and fill in
keys. `TestMain` walks up from the working directory to find `.env.test`; OS
environment variables take precedence over file values. Relevant variables:
`TEST_ANTHROPIC_API_KEY`, `TEST_OPENAI_API_KEY`, `TEST_GEMINI_API_KEY`,
`TEST_OPENAI_RESPONSE_API_KEY` (plus optional `*_BASE_URL` / `*_MODEL`
overrides). When a key is present, the corresponding mock test skips in favour
of the real call.

### 3. Service E2E tests (`internal/service/e2e/`, `//go:build e2e`)

Full HTTP request/response paths through the server:

- `responses_e2e_test.go`
- `anthropic_visual_e2e_test.go`
- `chat_visual_e2e_test.go`

### 4. Management API tests (`internal/service/api/`)

Endpoint behavior and integration for the management API and config graph
(`api_e2e_test.go`, `config_graph_test.go`, `*_test.go`).

## Live wire-shape verification matrix

A change is not considered verified until it has been exercised against the
running bridge in the consumer's exact wire shape (token =
`server.auth_token`):

1. **Codex shape** — `POST /v1/responses`, streaming, with a function tool and
   reasoning → expect a tool round-trip and `response.completed`.
2. **Claude Code shape** — `POST /v1/messages`, streaming **and** non-stream,
   with thinking and tools → expect the proper Anthropic SSE sequence /
   content blocks. Then run
   `claude -p "Reply with exactly: OK" --model sonnet` (and `haiku`).
3. **LibreChat / Affiora shape** — `POST /v1/chat/completions`, non-stream and
   streaming, with tools (arguments must be JSON **strings**) → verify from
   inside a consumer container, using the connection target that matches its
   network position (see the table in [CONSUMERS.md](CONSUMERS.md)).
4. **Models** — `GET /v1/models` → OpenAI `object`/`data[]` with slug ids,
   deduplicated against route aliases.
5. **Web search** — a `web_search` tool request through `/v1/messages` or
   `/chat/completions` → the injected Tavily search executes server-side (the
   answer contains fresh facts) and no `tool_use` blocks leak to the client.
6. **Image** — an image input through `/v1/chat/completions` (visual
   orchestration) → the image task is delegated to the configured vision
   provider and the answer returns normally; no visual tool calls leak to the
   consumer.

## Writing tests

- Use `httptest.NewServer` to mock upstream APIs.
- Protocol conversion tests verify round-trips through Core ⇄ protocol format.
- Add conversion unit tests for new adapters (canned Core events → asserted
  wire chunks) plus an E2E case.

## Coverage targets

- `internal/extension/plugin`: enforced ≥95% (`make cover-check`).
- Core protocol layer: keep high.
- Every new feature: accompanied by tests.
- E2E: cover every protocol path, including the two new inbounds and the
  visual path.
