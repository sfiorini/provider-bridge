# Provider Bridge CookBook

> A recipe collection: find the goal, follow the steps. Every recipe lists
> ingredients, steps, how to verify, and what to do when it fails.
>
> Commands are shown as paired blocks where the two platforms differ:
> **macOS / Linux** (bash) and **Windows** (PowerShell 7).

---

## Recipe Index

| # | Recipe | Time | Difficulty |
|---|--------|------|------------|
| 0 | [Before you start](#0-before-you-start) | 2 min | ⭐ |
| 1 | [First chat in 5 minutes](#1-first-chat-in-5-minutes) | 5 min | ⭐ |
| 2 | [Hook up the Codex CLI](#2-hook-up-the-codex-cli) | 3 min | ⭐⭐ |
| 3 | [Swap to another provider](#3-swap-to-another-provider) | 3 min | ⭐⭐ |
| 4 | [Turn on DeepSeek V4 reasoning](#4-turn-on-deepseek-v4-reasoning) | 2 min | ⭐ |
| 5 | [Let a text model see images (Visual extension)](#5-let-a-text-model-see-images-visual-extension) | 5 min | ⭐⭐⭐ |
| 6 | [Turn on web search](#6-turn-on-web-search) | 5 min | ⭐⭐ |
| 7 | [Enable the prompt cache](#7-enable-the-prompt-cache) | 2 min | ⭐ |
| 8 | [Troubleshooting quick reference](#8-troubleshooting-quick-reference) | — | — |

---

## 0. Before you start

**Ingredients:**

- **Go 1.25+** — check with `go version`. If missing, download it from
  [go.dev](https://go.dev/dl/), or on Windows install it with
  [Scoop](https://scoop.sh/) (`scoop install go`, no PATH editing needed).
- **An API key** — DeepSeek is a good first provider; create a key at
  [platform.deepseek.com](https://platform.deepseek.com).
- **A terminal** — PowerShell 7 is recommended on Windows.

Every recipe below runs the bridge from source with `go run ./cmd/providerbridge`.
If you have built the binary once with
`go build -o providerbridge ./cmd/providerbridge`, replace that prefix with
`./providerbridge` (or `.\providerbridge.exe` on Windows).

**Verify:**

**macOS / Linux**

```bash
go version
# go version go1.25.0 linux/amd64
```

**Windows (PowerShell)**

```powershell
go version
# go version go1.25.0 windows/amd64
```

**If it goes wrong:**

| Problem | Cause | Fix |
|---------|-------|-----|
| `command not found: go` | Go is not installed | Download it from [go.dev](https://go.dev/dl/) |
| `go: command not found` | Go is not on `PATH` | Restart the terminal after installing, or add Go's `bin` directory to `PATH` |

---

## 1. First chat in 5 minutes

**Goal:** send a message and get an AI reply.

**Ingredients:**

- [Recipe 0](#0-before-you-start) done
- A DeepSeek API key

**Steps:**

### 1.1 Create a config file

Start from the shipped example, then edit only `api_key`:

**macOS / Linux**

```bash
cp config.example.yml config.yml
```

**Windows (PowerShell)**

```powershell
Copy-Item .\config.example.yml .\config.yml
```

Config shape (v5):

```yaml
mode: "Transform"

server:
  addr: "127.0.0.1:38440"

models:
  deepseek-flash:
    context_window: 1000000
    max_output_tokens: 384000

providers:
  deepseek:
    protocol: "anthropic"
    base_url: "https://api.deepseek.com/anthropic"
    api_key: "sk-your-deepseek-key"
    offers:
      - model: deepseek-flash

routes:
  provider-bridge:
    model: deepseek-flash
    provider: deepseek

defaults:
  model: provider-bridge
  max_tokens: 4096
```

### 1.2 Start it

**macOS / Linux**

```bash
go run ./cmd/providerbridge -config ./config.yml
```

**Windows (PowerShell)**

```powershell
go run ./cmd/providerbridge -config ".\config.yml"
```

Wait for `Provider Bridge listening on 127.0.0.1:38440`. Leave this terminal
running and open a second one for the next step.

> Windows does not set `HOME` the way Unix shells do, so passing `-config`
> explicitly is required there. When `-config` is omitted the default is
> `$HOME/provider-bridge/config.yml`.

### 1.3 Test it

**macOS / Linux**

```bash
curl http://localhost:38440/v1/chat/completions \
  -H "Content-Type: application/json" \
  -d '{
    "model": "provider-bridge",
    "messages": [{"role": "user", "content": "Introduce yourself in one sentence."}],
    "max_tokens": 100
  }'
```

**Windows (PowerShell)**

```powershell
$body = @{
    model = "provider-bridge"
    messages = @(@{ role = "user"; content = "Introduce yourself in one sentence." })
    max_tokens = 100
} | ConvertTo-Json -Depth 5
Invoke-RestMethod -Uri http://localhost:38440/v1/chat/completions `
  -Method Post `
  -ContentType "application/json" `
  -Body $body
```

**Verify:** a JSON response with a `choices[0].message.content` reply.

The same model also answers the other inbound protocols — `POST /v1/responses`
(Codex-native, used in recipe 2) and `POST /v1/messages` (Anthropic Messages).

**If it goes wrong:**

| Problem | Cause | Fix |
|---------|-------|-----|
| `command not found: go` | Go is not installed | See recipe 0 |
| `connection refused` | The server is not running | Check the first terminal's output |
| `invalid yaml` / `cannot unmarshal` | Bad indentation | Use 2 spaces per level; never tabs |
| `401 unauthorized` | Wrong `api_key` | Check the key in the DeepSeek console |
| `402 payment required` | No balance | Top up at the DeepSeek console |
| The process exits with a Go error | Dependencies not downloaded yet | The first start needs network access |

---

## 2. Hook up the Codex CLI

**Goal:** Codex CLI calls DeepSeek through Provider Bridge.

**Ingredients:**

- Recipe 1 working
- Codex CLI installed (`npm install -g @openai/codex`)

**Steps:**

Provider Bridge ships a Codex config generator. First confirm the bridge is
up:

**macOS / Linux**

```bash
curl -s http://localhost:38440/v1/models | head -c 200
```

**Windows (PowerShell)**

```powershell
Invoke-RestMethod -Uri http://localhost:38440/v1/models | ConvertTo-Json -Depth 3
```

Then generate `config.toml` and `models_catalog.json`. On Windows, run the
commands **one at a time** — pasting them all at once can produce a malformed
TOML (for example `Error loading config.toml: unexpected key or value,
expected newline`).

**macOS / Linux**

```bash
CODEX_HOME_DIR="${CODEX_HOME:-$HOME/.codex}"
MODEL=$(go run ./cmd/providerbridge -config ./config.yml -print-codex-model)
go run ./cmd/providerbridge -config ./config.yml \
  -print-codex-config "$MODEL" \
  -codex-base-url "http://127.0.0.1:38440/v1" \
  -codex-home "$CODEX_HOME_DIR" \
  > "$CODEX_HOME_DIR/config.toml"
```

**Windows (PowerShell)**

```powershell
$CODEX_HOME_DIR = if ($env:CODEX_HOME) { $env:CODEX_HOME } else { "$HOME\.codex" }
```

```powershell
$MODEL = go run ./cmd/providerbridge -config ".\config.yml" -print-codex-model
```

```powershell
go run ./cmd/providerbridge -config ".\config.yml" `
  -print-codex-config "$MODEL" `
  -codex-base-url "http://127.0.0.1:38440/v1" `
  -codex-home "$CODEX_HOME_DIR" |
  Set-Content -Path "$CODEX_HOME_DIR\config.toml" -NoNewline
```

This writes two files under `$CODEX_HOME_DIR`:

- `config.toml` — Codex's model provider configuration
- `models_catalog.json` — model capabilities (context window, reasoning
  effort presets, tool types, …)

Start Codex:

**macOS / Linux**

```bash
CODEX_HOME="$CODEX_HOME_DIR" codex --cd "$PWD"
```

**Windows (PowerShell)**

```powershell
$env:CODEX_HOME = $CODEX_HOME_DIR; codex --cd $PWD
```

**Verify:** Codex starts; after a question, the Provider Bridge terminal logs
`POST /v1/responses`.

**If it goes wrong:**

| Problem | Cause | Fix |
|---------|-------|-----|
| `connection refused` | Provider Bridge is not running | Do recipe 1 first |
| `unexpected key or value` in `config.toml` | The generator output was truncated (commands pasted together) | Re-run the three commands one line at a time |
| An opaque startup error | The `CODEX_HOME` directory has no `models_catalog.json` | Check the path passed to `-codex-home` |

---

## 3. Swap to another provider

**Goal:** move from DeepSeek to another model (for example Anthropic).

**Ingredients:** recipe 1 working, plus the new provider's API key.

**Steps:**

Replace the `models`, `providers`, and `routes` blocks in `config.yml`:

```yaml
models:
  claude-sonnet-4-20250514:
    context_window: 200000
    max_output_tokens: 64000
    input_modalities: ["text", "image"]

providers:
  anthropic:
    protocol: "anthropic"
    base_url: "https://api.anthropic.com"
    api_key: "sk-ant-your-key"
    version: "2023-06-01"
    offers:
      - model: claude-sonnet-4-20250514

routes:
  provider-bridge:
    model: claude-sonnet-4-20250514
    provider: anthropic

defaults:
  model: provider-bridge
  max_tokens: 4096
```

Restart Provider Bridge (Ctrl+C, then start it again exactly as in recipe 1)
and repeat the curl test.

**Verify:** the same request now answers with Claude's voice.

> Swapping providers only edits `config.yml` — the Codex configuration does
> not change.

---

## 4. Turn on DeepSeek V4 reasoning

**Goal:** make DeepSeek V4's thinking mode (deep reasoning) available.

**Ingredients:** DeepSeek V4 model access + recipe 1 working.

**Steps:**

```yaml
extensions:
  deepseek_v4:
    config:
      reinforce_instructions: true
      reinforce_prompt: "[System Reminder]: Please pay close attention to the system instructions...\n[User]:"

models:
  deepseek-v4-pro:
    context_window: 1000000
    max_output_tokens: 384000
    default_reasoning_level: "high"
    supported_reasoning_levels:
      - effort: "high"
        description: "High reasoning effort"
      - effort: "xhigh"
        description: "Extra high reasoning effort"
    extensions:
      deepseek_v4:
        enabled: true

providers:
  deepseek:
    protocol: "anthropic"
    base_url: "https://api.deepseek.com/anthropic"
    api_key: "sk-your-key"
    offers:
      - model: deepseek-v4-pro

routes:
  provider-bridge:
    model: deepseek-v4-pro
    provider: deepseek

defaults:
  model: provider-bridge
  max_tokens: 4096
```

Restart Provider Bridge.

**Verify:** add `"reasoning": {"effort": "high"}` to the request; replies to
complex questions include a reasoning trace.

> `xhigh` maps to DeepSeek's `max` tier: deeper thinking, but slower and more
> expensive.

---

## 5. Let a text model see images (Visual extension)

**Goal:** a text-only main model (such as DeepSeek) delegates image inputs to
a dedicated vision model through the Visual extension.

**Ingredients:**

- Recipe 1 working
- A vision-model provider that speaks the Anthropic protocol (for example
  Kimi at `https://api.kimi.com/coding/`)
- Two API keys: main model + vision model

**Steps:**

```yaml
extensions:
  visual:
    config:
      provider: "kimi"
      model: "kimi-for-coding"
      max_rounds: 4
      max_tokens: 2048

models:
  deepseek-v4-pro:
    context_window: 1000000
    extensions:
      deepseek_v4:
        enabled: true
      visual:
        enabled: true
  kimi-for-coding:
    context_window: 128000
    input_modalities: ["text", "image"]

providers:
  deepseek:
    protocol: "anthropic"
    base_url: "https://api.deepseek.com/anthropic"
    api_key: "sk-your-deepseek-key"
    offers:
      - model: deepseek-v4-pro
  kimi:
    protocol: "anthropic"
    base_url: "https://api.kimi.com/coding/"
    api_key: "sk-your-kimi-key"
    offers:
      - model: kimi-for-coding

routes:
  provider-bridge:
    model: deepseek-v4-pro
    provider: deepseek

defaults:
  model: provider-bridge
  max_tokens: 4096
```

Restart Provider Bridge.

**Verify:** send a request that contains an image; the model describes it.

---

## 6. Turn on web search

**Goal:** let the model search the web.

**Ingredients:** recipe 1 working + a Tavily API key (free at
[tavily.com](https://tavily.com)).

**Steps:**

```yaml
web_search:
  support: "injected"
  tavily_api_key: "tvly-your-key"
  search_max_rounds: 5

models:
  deepseek-flash:
    context_window: 1000000

providers:
  deepseek:
    protocol: "anthropic"
    base_url: "https://api.deepseek.com/anthropic"
    api_key: "sk-your-key"
    offers:
      - model: deepseek-flash

routes:
  provider-bridge:
    model: deepseek-flash
    provider: deepseek

defaults:
  model: provider-bridge
  max_tokens: 4096
```

Restart Provider Bridge.

**Verify:** ask a time-sensitive question (for example "what is the weather
today?"); the reply should include search sources.

> `support` values: `auto` (detect), `enabled` (force the provider's native
> search), `disabled` (off), `injected` (run Tavily/Firecrawl inside the
> bridge, independent of the provider).
>
> Web-search resolution happens **at startup**: changing any `web_search`
> setting requires a restart of Provider Bridge.

---

## 7. Enable the prompt cache

**Goal:** reduce the cost of repeated input.

**Ingredients:** a provider that speaks the Anthropic protocol.

**Steps:**

```yaml
cache:
  mode: "explicit"
  ttl: "5m"
```

Add it at the top level of `config.yml` and restart Provider Bridge.

> `mode` values: `off`, `automatic`, `explicit` (manual breakpoints,
> recommended), `hybrid` (all on).

---

## 8. Troubleshooting quick reference

### YAML indentation

Use 2 spaces, never tabs:

```yaml
# wrong
provider:
    base_url: "..."    # 4 spaces

# right
provider:
  base_url: "..."      # 2 spaces
```

### The server will not start

**macOS / Linux**

```bash
go run ./cmd/providerbridge -config /path/to/config.yml 2>&1 | head -30
```

**Windows (PowerShell)**

```powershell
go run ./cmd/providerbridge -config "C:\path\to\config.yml" 2>&1 | Select-Object -First 30
```

| Error | Cause |
|-------|-------|
| `no such file or directory` | Wrong `config.yml` path |
| `cannot unmarshal` | Malformed YAML |
| `unsupported protocol` | `protocol` must be one of `anthropic`, `openai-response`, `google-genai`, `openai-chat` |
| `connection refused` | The provider's `base_url` is wrong or unreachable |
| `401` / `403` | Wrong API key |
| `402` | DeepSeek balance exhausted |
| `rate limit` | Too many requests |

### curl does not work

**macOS / Linux**

```bash
curl -s http://localhost:38440/v1/models | head -c 200
```

**Windows (PowerShell)**

```powershell
Invoke-RestMethod -Uri http://localhost:38440/v1/models | ConvertTo-Json -Depth 3
```

No output means Provider Bridge is not running; output but a failing request
means the model name is wrong.

### Visual does not work

- Does the provider named in `extensions.visual.config.provider` exist?
- Does the vision provider speak the Anthropic protocol?
- Is `visual.enabled: true` set on the main model?

---

## Contributing a recipe

Got a configuration combination you use often? Open a PR and add a recipe.
