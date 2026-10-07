# Model Inventory

The deployed model inventory for Provider Bridge: every model slug that consumers can
route to, its provider-verified context window / max output tokens / input modalities,
and the verification status of each row.

**Risk direction.** Under-declaring a context window causes premature compaction;
over-declaring causes late compaction → upstream context-overflow errors. Round DOWN on
ambiguity.

**Status vocabulary:** `verified` (provider-documented), `corrected` (was wrong, now
fixed), `conservative-unverified` (rounded down pending confirmation), `retired-alias`
(upstream retired the model name).

Checked against `GET /v1/models` on a live deployment and the upstream providers'
own `/v1/models` metadata. Last checked 2026-10-07.

## Models

`/v1/models` serves exactly these six (`data[]`, deduplicated against route aliases):

| Slug (route aliases) | Provider | Upstream model | Context window | Max output | Input modalities | Status | Source | Checked |
|---|---|---|---|---|---|---|---|---|
| `mistral-large-4` | mistral | mistral-large-4 | 1000000 | 262144 | text, image | verified | docs.mistral.ai/models/mistral-large-4-0 (public preview v26.10); live `api.mistral.ai/v1/models`; `max_tokens` ceiling probed at 262144 | 2026-10-07 |
| `zai-glm-5-3` | mistral | glm-5.3 | 1000000 | 131072 | text | verified | docs.z.ai/guides/llm/glm-5.3; docs.mistral.ai/inference/model-selection-guide | 2026-10-07 |
| `zai-glm-5-2` | mistral | glm-5.2 | 1000000 | 131072 | text | verified | docs.z.ai/guides/llm/glm-5.2 (deprecated by Mistral 2026-09-29) | 2026-10-07 |
| `deepseek-v4-pro` | deepseek | deepseek-v4-pro | 1000000 | 384000 | text | verified | live `api.deepseek.com/v1/models` (`input_modalities: ["text"]`) | 2026-10-07 |
| `deepseek-flash` (was `deepseek-v4-flash`) | deepseek | DeepSeek-V4.1-Flash | 1000000 | 384000 | **text, image** | corrected | live `api.deepseek.com/v1/models` → `input_modalities: ["text","image"]`, ctx 1048576, max_out 393216 (bridge rounds down to 1000000/384000); 64×64 red-PNG probe → "Red" | 2026-10-07 |
| `space-bunny-free` | zen | space-bunny-free | 128000 | — (undocumented) | text | conservative-unverified | opencode.ai/docs/zen (no context length published) | 2026-10-07 |

**`mistral-large-4` operational notes:** accepts only `reasoning_effort: high` (low/medium/xhigh/max → HTTP 400) and returns block-array `[thinking, text]` content. Pricing is a public-preview launch discount: input $0.68/M, cached input $0.07/M, output $2.09/M.

**`mistral-large-4` is the global visual vision model** (`extensions.visual.config.model`), since it is the only reliably multimodal Mistral model in the deployment.

## Claude Code route aliases

These aliases resolve to the rows above (they carry no independent metadata).

| Slug (route aliases) | Provider | Upstream model | Context window | Max output | Input modalities | Status | Source | Checked |
|---|---|---|---|---|---|---|---|---|
| `opus` (Claude Code route) | mistral | zai-glm-5-3 | 1000000 | 131072 | text | verified (resolves to `zai-glm-5-3`) | see `zai-glm-5-3` | 2026-10-07 |
| `sonnet` (Claude Code route) | deepseek | deepseek-v4-pro | 1000000 | 384000 | text | verified (resolves to `deepseek-v4-pro`) | see `deepseek-v4-pro` | 2026-10-07 |
| `haiku` (Claude Code route) | zen | space-bunny-free | 128000 | — (undocumented) | text | conservative-unverified (resolves to `space-bunny-free`) | see `space-bunny-free` | 2026-10-07 |

## Removed from the deployment

Recorded for rollback context. Removed 2026-10-07 so that the `mistral` provider exposes
only `mistral-large-4` and `zai-glm-*`:

| Slug | Reason it was removed | Notes |
|---|---|---|
| `mistral-medium-3.5` (+ alias route `mistral-medium-3-5`) | operator decision (prune `mistral/`) | was the global `visual` vision model; vision model repointed to `mistral-large-4` first |
| `mistral-large-latest` | operator decision (prune `mistral/`) | upstream aliases `mistral-large-2512`; the API advertises `vision: true` but a red-PNG probe answered "White", so it was kept text-only and is not reliable for images |
| `magistral-medium-latest` | operator decision (prune `mistral/`) | upstream resolves to `mistral-medium-3-5`; is genuinely multimodal |
| `devstral-latest` | operator decision (prune `mistral/`) | retired alias: absent from Mistral's model list, live calls are served by `mistral-medium-3-5` (ctx 262144, multimodal) |
| `codestral-latest` | operator decision (prune `mistral/`) | text-only |
| `mistral-large-4-0` | operator decision (prune `mistral/`) | Mistral's own alias for `mistral-large-4`; removed to keep the list minimal |
| `kimi-for-coding` | never deployed | appears in `config.example.yml` only |

## Corrections applied

**2026-10-07**
- `deepseek-flash`: `input_modalities` `text` → **`text, image`**. Upstream declares it multimodal; the bridge was declaring it text-only, so the capability gate stripped/orchestrated images instead of forwarding them.
- `magistral-medium-latest`: `input_modalities` `text` → `text, image` (upstream `vision: true`, red-PNG probe "Red"); removed later the same day by the prune.
- `mistral-large-4`: added (`context_window` 1000000, `max_output_tokens` 262144, `text, image`, pricing above).
- `mistral-medium-3.5`, `mistral-large-latest`, `devstral-latest`, `magistral-medium-latest`, `codestral-latest`: removed by the prune (see above).
- **Known limitation (code, tracked):** images sent to an **anthropic-protocol** upstream (DeepSeek) fail with HTTP 400 `base64 decode error` when they arrive from the chat/responses inbounds, because the Core image block carries a full `data:` URL while Anthropic requires raw base64. Fixed by normalising the image source in the anthropic/DeepSeek/Google upstream adapters (`format.SplitImageSource`).
- **Known limitation (code, tracked):** the visual-assist tools were never offered on the Core executor paths (`/v1/messages`, `/v1/chat/completions`) — only the Responses inbound injected them — so the orchestrator could not be triggered. Fixed by having the orchestrator offer its own tools.

**2026-10-06**
- `deepseek-v4-flash`: renamed to `deepseek-flash` (upstream retired the model name; requests are served by DeepSeek-V4.1-Flash).
- `devstral-latest`: 262144 → 131072 (was 2× too high; corrected to the Devstral Medium window).
- `mistral-large-latest`: 262144 → 131072 (conservative).
- `space-bunny-free`: stays 128000, unverified (OpenCode Zen publishes no context length).
