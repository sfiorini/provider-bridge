# Contributing to Provider Bridge

Thanks for your interest in Provider Bridge. Issues and pull requests are
welcome.

## Reporting issues

Open a GitHub Issue and include:

- Environment: OS, how the bridge is run (binary, Docker, Cloudflare), version
  or commit.
- Configuration: the relevant `config.yml` / config-graph sections, **with
  secrets masked**.
- Steps to reproduce, the expected behavior, and the actual behavior.
- For API errors: the status code, the response body, and — if available — the
  request trace (enable `trace.enabled: true`).

## Branch policy

The repository is **`main`-only**. `main` is the single long-lived branch: it is
stable, and every change lands there. There is no `dev` branch.

- `main` — the integration branch; all work merges here.
- `feat/*` — feature branches, cut from `main` and merged back into `main`.
- `fix/*` — bug-fix branches, cut from `main` and merged back into `main`.

Keep branches short-lived and rebase on `main` before opening a pull request.

## Development flow

1. Fork the repository (or branch from `main` if you have write access) and
   create a topic branch: `git checkout -b feat/my-feature`.
2. Write the code and the tests together — no untested behavior.
3. Run the test suites (see [Testing](#testing) below).
4. Open a pull request **into `main`** with a description of the change and the
   verification you ran.
5. A maintainer reviews and merges.

Development setup, the project tree, and the build/test commands are documented
in [docs/DEVELOPMENT.md](docs/DEVELOPMENT.md). The mini-host build/test commands
(the canonical dev environment) and the test tiers live in
[docs/TESTING.md](docs/TESTING.md).

## Code standards

The authoritative conventions are in
[docs/DEVELOPMENT-CONVENTIONS.md](docs/DEVELOPMENT-CONVENTIONS.md). In short:

- Use `log/slog` for structured logging; **error and log messages are English**
  (the production catalog is the same source of truth as
  `docs/DEVELOPMENT-CONVENTIONS.md`).
- Name files after the responsibility (for example
  `candidate_routing_test.go`), not after a project-management number.
- All protocol conversion goes through the Core intermediate representation
  (`format.CoreRequest` / `format.CoreResponse`).
- `internal/protocol/*` must not import `internal/service` or
  `internal/extension` — dependency direction is enforced by review.
- Keep commits focused; one story or fix per commit.

## Testing

See [docs/TESTING.md](docs/TESTING.md) for the full tier list (unit tests,
`internal/e2e/` behind the `e2e` build tag, `internal/service/e2e/` full-HTTP
tests, and the management API). Minimum expectations:

- Unit-test new code, including protocol conversion (canned Core events →
  assert the emitted wire payloads).
- Add or extend an end-to-end test for new adapters and new request paths.
- Run the suites before opening a pull request:

  ```bash
  go test ./...
  go test -tags=e2e ./internal/e2e/... ./internal/service/e2e/...
  ```

- For any request-path change, also verify a **live streaming request in the
  consumer's exact wire shape** (see the verification matrix in
  [docs/TESTING.md](docs/TESTING.md)).

## Adding a provider adapter

A provider adapter converts Core to an upstream wire protocol and back.

1. Add the protocol constant in `internal/config/config.go` (for example
   `ProtocolMyAdapter`).
2. Create `internal/protocol/<adapter>/` and implement both
   `format.ProviderAdapter` (`FromCoreRequest` / `ToCoreResponse`) and
   `format.ProviderStreamAdapter` (`ToCoreStream` → `StreamResult`). See
   `internal/format/adapter.go` for the exact interfaces.
3. Register the adapter in the `format.Registry` wiring in
   `internal/service/app/app.go`.
4. Add the protocol's dispatch branch in
   `internal/service/server/adapter_dispatch.go` (type-assert the produced wire
   request and pick the HTTP client).
5. Add unit tests for the conversion and an end-to-end test under
   `internal/e2e/`, then run the [testing](#testing) commands.

## Adding an inbound ClientAdapter

An inbound ClientAdapter lets a consumer speak a new wire protocol; the bridge
converts it to Core early and then reuses the shared upstream executor.

1. Create `internal/protocol/<client>/client_adapter.go` and implement
   `format.ClientAdapter` (`ToCoreRequest` / `FromCoreResponse`). For streaming,
   also implement `format.ClientStreamAdapter` (`FromCoreStream` → a
   protocol-specific stream result plus a trace buffer).
2. Add the HTTP handler (and any token-count style companion endpoint) in
   `internal/service/server/inbound_handlers.go`, and register the route in
   `internal/service/server/server.go`.
3. Register the inbound adapter in the `format.Registry` in
   `internal/service/app/app.go` (see the `anthropic-messages` and
   `chat-completions` entries).
4. Route the handler through the shared `executeCoreUpstream`
   (`internal/service/server/core_upstream.go`) so web-search injection, visual
   orchestration, reasoning replay, tracing, and usage stats are reused — do not
   reimplement them.
5. Mind the wire-level invariants in `AGENTS.md` §2.3 (tool results as Core role
   `tool`; Chat `arguments` as JSON strings; empty `json.RawMessage`;
   `CoreToolCallArgsDone` on the visual path; both stream synthesizers).
6. Add conversion unit tests (canned Core events → assert emitted wire chunks)
   and a live streaming verification in the consumer's exact wire shape.

## License

This project is licensed under [GPL v3](LICENSE). By contributing, you agree
that your contributions are released under the same license.
