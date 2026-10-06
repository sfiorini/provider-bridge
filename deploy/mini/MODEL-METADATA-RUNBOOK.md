# Model Metadata Reconciliation Runbook (mini)

Executable, copy-pasteable procedure to bring the **live mini deployment** into
agreement with the verified values in [`INVENTORY.md`](../../INVENTORY.md).
Run all commands on **mini**; `TOKEN` is `server.auth_token` from
`/opt/docker/provider-bridge/config.yml`, and the admin API base URL is
`http://localhost:38440`.

```bash
TOKEN="$(grep -m1 'auth_token:' /opt/docker/provider-bridge/config.yml | awk '{print $2}')"
BASE="http://localhost:38440"
```

Related tracked files: [`codex_regen.sh`](./codex_regen.sh) (step 4),
[`PATCHES.md`](./PATCHES.md), [`update.sh`](./update.sh).

---

## 0. Backup first

```bash
sudo cp /opt/docker/provider-bridge/config.yml \
        /opt/docker/provider-bridge/config.yml.bak-$(date +%F)
```

## 1. Read the current graph revision

```bash
curl -s -H "Authorization: Bearer $TOKEN" "$BASE/api/v1/config/graph" | jq .revision
```

Note the value as `REV`. Every PATCH must use the revision that was current when
it was built; re-read after each commit (the revision increments).

## 2. Apply the metadata change set (hot-reload, no restart)

Models' `context_window` / `max_output_tokens` / `input_modalities` are
`HotReloadable:true` — expect `{"result":"committed",…}` and **no** container
restart.

Patch body contract (verbatim):

```json
{"baseRevision":"<REV>","changes":[{"kind":"model","id":"<slug>","field":"<context_window|max_output_tokens|input_modalities>","value":<number-or-array>}]}
```

One field per call. Success is `{"result":"committed"}`; a revision conflict is
resolved by re-reading step 1 and re-submitting the same change.

```bash
curl -s -X PATCH -H "Authorization: Bearer $TOKEN" -H "Content-Type: application/json" \
  "$BASE/api/v1/config/graph" -d \
  '{"baseRevision":"<REV>","changes":[{"kind":"model","id":"<slug>","field":"<field>","value":<value>}]}'
```

Change set (from INVENTORY.md), each as one PATCH:

| Slug | Field | Value |
|---|---|---|
| `zai-glm-5-3` | `max_output_tokens` | `131072` |
| `zai-glm-5-2` | `max_output_tokens` | `131072` |
| `deepseek-v4-pro` | `max_output_tokens` | `384000` |
| `mistral-medium-3.5` | `max_output_tokens` | `209715` |
| `mistral-large-latest` | `context_window` | `131072` |
| `devstral-latest` | `context_window` | `131072` |
| `magistral-medium-latest` | `context_window` | `128000` |
| `space-bunny-free` | `context_window` | `128000` |
| every text-only model above | `input_modalities` | `["text"]` |

Worked example:

```bash
curl -s -X PATCH -H "Authorization: Bearer $TOKEN" -H "Content-Type: application/json" \
  "$BASE/api/v1/config/graph" -d \
  '{"baseRevision":"<REV>","changes":[{"kind":"model","id":"devstral-latest","field":"context_window","value":131072}]}'
```

Then re-read the revision (step 1) before the next PATCH.

## 3. Rename `deepseek-v4-flash` → `deepseek-flash`

The upstream retired the old name (see INVENTORY.md). Sequence:

**(a)** Create the new model, copying the old model's metadata under the new slug:

```bash
curl -s -X POST -H "Authorization: Bearer $TOKEN" -H "Content-Type: application/json" \
  "$BASE/api/v1/config/resources/model" -d \
  '{"id":"deepseek-flash","value":{"context_window":1000000,"max_output_tokens":384000,"input_modalities":["text"]}}'
```

**(b)** Re-point the `deepseek` provider's offer model name `deepseek-v4-flash` →
`deepseek-flash` (field path on the provider resource), then commit via the graph
PATCH endpoint.

**(c)** Re-point any route whose `model` references `deepseek-v4-flash`.

**(d)** Delete the old model:

```bash
curl -s -X DELETE -H "Authorization: Bearer $TOKEN" \
  "$BASE/api/v1/config/resources/model/deepseek-v4-flash"
```

**(e)** Verify the rename:

```bash
curl -s -o /dev/null -w '%{http_code}\n' -H "Authorization: Bearer $TOKEN" "$BASE/api/v1/models/deepseek-flash"        # 200
curl -s -o /dev/null -w '%{http_code}\n' -H "Authorization: Bearer $TOKEN" "$BASE/api/v1/models/deepseek-v4-flash"    # 404
```

If step (d) returns 409, the offer re-point in (b) did not commit — re-check the
graph with `GET $BASE/api/v1/config/graph` before retrying.

## 4. Regenerate the Codex catalog

The graph reload does **not** regenerate the Codex catalog — this is a separate
step:

```bash
cd /opt/docker/provider-bridge && sudo ./codex_regen.sh
```

Then restart Codex on the machine that consumed the old catalog.

## 5. Mirror `config.yml`

Export the live graph back over the seed/mirror file (secrets included), keeping
mode `0644` (the container reads it as nonroot `65532`):

```bash
curl -s -H "Authorization: Bearer $TOKEN" \
  -H "X-Confirm-Secrets: true" \
  "$BASE/api/v1/config/export?include_secrets=true" \
  > /opt/docker/provider-bridge/config.yml
sudo chmod 644 /opt/docker/provider-bridge/config.yml
```

## 6. Mac side

1. Set `CLAUDE_CODE_MAX_CONTEXT_TOKENS` in Claude Code settings (manual by
   design — automation is out of scope). Current user value: `1000000`.
2. Regenerate the Mac `~/.codex/config.toml` (and `models_catalog.json`): either
   run the same `--codex-home` generation or copy mini's
   `generated_configs/` output. This is required because the Codex provider
   key/model set changed (the `deepseek-flash` rename).

## 7. Verify

```bash
for slug in deepseek-flash zai-glm-5-3 devstral-latest; do
  echo "== $slug =="
  curl -s -H "Authorization: Bearer $TOKEN" "$BASE/api/v1/models/$slug" \
    | jq '.context_window, .max_output_tokens, .input_modalities'
done
```

- `/api/v1/models/<slug>` shows the INVENTORY.md values. **`GET /v1/models` does
  NOT expose `context_window`** — always verify through `/api/v1/models/{slug}`.
- The regenerated `config.toml` carries `model_context_window` /
  `model_max_output_tokens` lines matching INVENTORY.md.
- One live Codex session and one live Claude Code session succeed
  (`claude -p "Reply with exactly: OK" --model sonnet`, and haiku).

## Notes and constraints

- `context_window` hot-reloads; **`web_search` config is startup-only** and needs
  a container restart.
- Run steps during a quiet window: the Mac `moonbridge-zen-sync` LaunchAgent
  mirrors the same graph and can cause revision conflicts.

## Rollback

Every PATCH is hot-reloadable and reversible by patching the field back to its
previous value. The `config.yml.bak-<date>` copy from step 0 is the pre-change
snapshot; restore it and regenerate the catalog (step 4) to revert the mirror.
The `deepseek-flash` rename is reversed by recreating the old slug and deleting
the new one in the opposite order.
