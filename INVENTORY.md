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

## Models

| Slug (route aliases) | Provider | Upstream model | Context window | Max output | Input modalities | Status | Source | Checked |
|---|---|---|---|---|---|---|---|---|
| `zai-glm-5-3` | mistral | glm-5.3 | 1000000 | 131072 | text | verified | docs.z.ai/guides/llm/glm-5.3; docs.mistral.ai/inference/model-selection-guide | 2026-10-06 |
| `zai-glm-5-2` | mistral | glm-5.2 | 1000000 | 131072 | text | verified | docs.z.ai/guides/llm/glm-5.2 (deprecated by Mistral 2026-09-29) | 2026-10-06 |
| `deepseek-v4-pro` | deepseek | deepseek-v4-pro | 1000000 | 384000 | text | verified | api-docs.deepseek.com/quick_start/pricing | 2026-10-06 |
| `deepseek-flash` (was `deepseek-v4-flash`) | deepseek | deepseek-flash | 1000000 | 384000 | text | retired-alias → corrected | api-docs.deepseek.com/quick_start/pricing footnote: "Use `deepseek-flash` as the model name. The legacy names `deepseek-v4-flash` and `deepseek-v4-flash-vision-exp` are still accepted, but the corresponding models have been retired…" | 2026-10-06 |
| `mistral-medium-3.5` | mistral | mistral-medium-3-5-26-04 | 262144 | 209715 | text, image | verified (modalities: live graph; output: 3rd-party) | docs.mistral.ai/models/mistral-medium-3-5-26-04; contextwindows.dev/models/mistral-medium-3-5; live graph (global `visual` vision model) | 2026-10-06 |
| `mistral-large-latest` | mistral | mistral-large-3 | 131072 | 8191 | text | conservative-unverified (raise to 262144 only after confirming alias=Large 3) | contextwindows.dev/models/mistral-large-3; docs.mistral.ai/models/mistral-large-3-25-12 | 2026-10-06 |
| `devstral-latest` | mistral | devstral-medium | 131072 | — (undocumented; leave unset) | text | corrected (was 262144, 2× too high) | contextwindows.dev/models/devstral-medium | 2026-10-06 |
| `magistral-medium-latest` | mistral | magistral-medium | 128000 | 8192 | text | conservative-unverified | llm-stats.com/models/magistral-medium; contextwindows.dev/models/mistral-magistral-small-2509 | 2026-10-06 |
| `space-bunny-free` | zen | space-bunny-free | 128000 | — (undocumented) | text | conservative-unverified | opencode.ai/docs/zen (no context length published) | 2026-10-06 |
| `kimi-for-coding` | kimi | kimi-for-coding | — (undocumented) | — | text | conservative-unverified | api.kimi.com (coding model) | 2026-10-06 |

## Claude Code route aliases

These aliases resolve to the rows above (they carry no independent metadata).

| Slug (route aliases) | Provider | Upstream model | Context window | Max output | Input modalities | Status | Source | Checked |
|---|---|---|---|---|---|---|---|---|
| `opus` (Claude Code route) | mistral | zai-glm-5-3 | 1000000 | 131072 | text | verified (resolves to `zai-glm-5-3`) | see `zai-glm-5-3` | 2026-10-06 |
| `sonnet` (Claude Code route) | deepseek | deepseek-v4-pro | 1000000 | 384000 | text | verified (resolves to `deepseek-v4-pro`) | see `deepseek-v4-pro` | 2026-10-06 |
| `haiku` (Claude Code route) | zen | space-bunny-free | 128000 | — (undocumented) | text | conservative-unverified (resolves to `space-bunny-free`) | see `space-bunny-free` | 2026-10-06 |

## Corrections applied

- `devstral-latest`: 262144 → 131072 (was 2× too high; corrected to the Devstral Medium window).
- `mistral-large-latest`: 262144 → 131072 (conservative; raise to 262144 only after confirming the `-latest` alias targets Large 3).
- `space-bunny-free`: stays 128000, unverified (OpenCode Zen publishes no context length).
- `deepseek-v4-flash`: renamed to `deepseek-flash` (upstream retired the model name; requests are served by DeepSeek-V4.1-Flash).
- `mistral-medium-3.5`: `input_modalities` is `text, image`, not `text` — it is the global `visual` vision model (`provider=mistral`, `model=mistral-medium-3.5`); marking it text-only would make the capability gate strip images from direct requests. The graph was left unchanged; only this row was corrected.
