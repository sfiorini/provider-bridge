# Provider Bridge — runtime rename cutover (legacy name → providerbridge)

> The legacy product/binary name is deliberately **not** reproduced in this
> repo: the branding gate permits a single mention (the README attribution
> line). Every occurrence of the old name below is written as `<legacy>`, and
> the legacy env prefix as `<LEGACY>_*`; the runbook is otherwise verbatim.

Maintenance-window runbook for S-M3-6/S-M3-7. Executes the branding scrub on the
live deployment: mini host `mini` (`/opt/docker/provider-bridge`) and the Mac
LaunchAgents/scripts. **Config is frozen for the whole window — no config-graph
edits while the cutover runs.**

The rename is a *naming* change only: filenames, binary, env prefixes, provider
key and LaunchAgent labels. The SQLite config graph content and the model
inventory are untouched.

## Preconditions

- New image buildable on mini: `docker compose build` from the renamed repo
  (worktree `flow/2026-10-06-provider-bridge-remediation`).
- Announce the window (service restart + brief Codex/Claude downtime).
- Snapshot `/opt/docker/provider-bridge/config.yml` and `data/` **before**
  touching anything (see Backups below).
- Record the running image id so the pre-cutover image can be tagged for
  rollback: `docker inspect -f '{{.Image}}' provider-bridge`.

## Backups (do first)

```sh
# mini
cd /opt/docker/provider-bridge
sudo cp -a docker-compose.yml docker-compose.yml.bak-<date>-m3
sudo cp -a config.yml       config.yml.bak-<date>-m3
sudo cp -a data             data.bak-<date>-m3          # db + -wal + -shm
IMG=$(sudo docker inspect -f '{{.Image}}' provider-bridge)
sudo docker tag "$IMG" provider-bridge:rollback-m3      # rollback image
sudo docker inspect -f '{{.State.StartedAt}}' provider-bridge

# Mac
cp -a ~/Library/LaunchAgents/com.fiorinis.<legacy>-tunnel.plist  *.bak-<date>-m3
cp -a ~/Library/LaunchAgents/com.fiorinis.<legacy>-zen-sync.plist *.bak-<date>-m3
cp -a ~/.local/bin/<legacy>-zen-sync                              *.bak-<date>-m3
cp -a ~/.codex/config.toml ~/.codex/models_catalog.json             *.bak-<date>-m3
```

## Mini steps

1. **Deploy the renamed tree** (worktree is canonical; nothing is pushed to
   GitHub):
   ```sh
   rsync -a --delete --exclude .git --exclude .kilo <worktree>/ mini:/tmp/pb-build/
   ssh mini 'sudo rsync -a --delete /tmp/pb-build/ /opt/docker/provider-bridge/src/'
   ```
2. **Build the new image** — must produce `/app/providerbridge`:
   ```sh
   ssh mini 'cd /opt/docker/provider-bridge && sudo docker compose build'
   ```
3. **Stop the container:**
   ```sh
   ssh mini 'cd /opt/docker/provider-bridge && sudo docker compose stop'
   ```
4. **Rename the DB with its `-wal`/`-shm` siblings** (a clean stop checkpoints
   the WAL, but move the siblings anyway):
   ```sh
   cd /opt/docker/provider-bridge/data
   mv <legacy>.db provider-bridge.db
   mv <legacy>.db-wal  provider-bridge.db-wal   # if present
   mv <legacy>.db-shm  provider-bridge.db-shm   # if present
   ```
   > Copying `<legacy>.db` without its `-wal`/`-shm` siblings loses recent
   > transactions when the source is running; stop the source container first
   > (a clean close checkpoints the WAL).

   **Also update the DB path in `config.yml`**
   (`db_sqlite.path: /app/data/<legacy>.db` → `/app/data/provider-bridge.db`)
   or the service starts against a fresh empty graph.
5. **Update `/opt/docker/provider-bridge/docker-compose.yml`:** healthcheck
   `test:` first element `/app/<legacy>` → `/app/providerbridge`; any other
   old binary/image/container/env reference from the inventory. Keep
   `command: -config /config/config.yml -addr 0.0.0.0:38440` and the volume
   mounts. The service/container stay `provider-bridge` (the network alias
   consumers like affiora use); only the binary path changes.
6. **Rename the old runtime dirs** (defensive; report if absent):
   ```sh
   mv ~/.<legacy> ~/.provider-bridge            # if it exists
   mv .<legacy>.env .provider-bridge.env        # if it exists
   ```
7. **Install the repo's `deploy/mini/` files over the mini copies** (repo
   becomes canonical): `PATCHES.md`, `update.sh`, `codex_regen.sh`,
   `MODEL-METADATA-RUNBOOK.md`; preserve executables at `0755` and confirm
   `codex_regen.sh`/`update.sh` reference the new names.
8. **Start:** `ssh mini 'cd /opt/docker/provider-bridge && sudo docker compose up -d'`
   → wait for healthy.
9. **Codex regen:** `ssh mini 'sudo /opt/docker/provider-bridge/codex_regen.sh'`
   → the regenerated `config.toml` must emit `model_provider = "provider-bridge"`
   and `[model_providers.provider-bridge]`. Restart the Codex app-server.
10. **Verify:** healthcheck green; `/v1/models` OpenAI `object`/`data[]`;
    a Codex-shaped `/v1/responses` streaming call returns `response.completed`;
    `claude -p "Reply with exactly: OK" --model sonnet` and `--model haiku`
    succeed; the AGENTS.md §4.1 matrix (5 rows) plus the M1 image row.

## Mac steps

11. **LaunchAgents:** `launchctl unload` + delete
    `com.fiorinis.<legacy>-tunnel` and `com.fiorinis.<legacy>-zen-sync`;
    install renamed equivalents `com.fiorinis.provider-bridge-tunnel` /
    `com.fiorinis.provider-bridge-zen-sync` (Label + ProgramArguments + env
    paths updated), `launchctl load` them.
12. **Zen sync script:** `mv ~/.local/bin/<legacy>-zen-sync
    ~/.local/bin/provider-bridge-zen-sync`; edit contents — env prefix
    `<LEGACY>_*` → `PROVIDER_BRIDGE_*`, codex provider key `provider-bridge`,
    stale `sudo docker logs <legacy>` → `provider-bridge`, and every remaining
    occurrence of the old name → the new name: search both the hyphenated
    product spelling (`<legacy-name>`) and the un-hyphenated binary/env
    spelling (`<legacy-binary>`). Confirm it parses:
    `python3 -m py_compile ~/.local/bin/provider-bridge-zen-sync`.
13. **Codex config:** regenerate `~/.codex/config.toml` +
    `models_catalog.json` so Codex uses `provider-bridge`. If regenerating,
    preserve the hand-maintained personality/projects/MCP sections — do not
    clobber them silently; report exactly what was changed.

## Verification

- Healthcheck green (`docker compose ps`).
- Full AGENTS.md §4.1 matrix (Codex, Claude Code stream + non-stream, LibreChat/
  Affiora chat-completions with tools, `/v1/models`, web search) plus the M1
  image row.
- Post-cutover grep on mini (excluding the repo checkout) → 0 hits (backup
  files excepted):
  ```sh
  sudo grep -riE 'moon[-_ ]?bridge|zhiyi-?r' /opt/docker/provider-bridge/ --exclude-dir=src
  ```
- Same grep on Mac for `~/.local/bin/provider-bridge-zen-sync`,
  `~/Library/LaunchAgents/`, `~/.codex/config.toml` → 0 hits.

## Accepted breakages

- Console localStorage reset — users log in again.
- Stale Codex `config.toml` fails until regenerated.
- External scripts referencing `<LEGACY>_*` or `/app/<legacy>` break loudly.

## Rollback

Stop the container, restore `docker-compose.yml` and the `data/<legacy>.db*`
names, retag the pre-cutover image back to `provider-bridge:latest`
(`docker tag provider-bridge:rollback-m3 provider-bridge:latest`), restore the
old plists/script, and `docker compose up -d`. The config graph content is
untouched by the rename (filenames only) — no data migration to roll back.

## Execution log (S-M3-7)

Executed 2026-10-06 on `mini` and the Mac, one maintenance window, config frozen.

**Backups (pre-cutover)**
- mini: `docker-compose.yml.bak-2026-10-06-m3`, `config.yml.bak-2026-10-06-m3`,
  `data.bak-2026-10-06-m3/` (db 798720 B + `-shm` 32768 B + `-wal` 852872 B).
- Running image recorded: `sha256:ff23e96ba4ab…` (StartedAt `2026-10-06T15:00:25Z`);
  tagged `provider-bridge:rollback-m3` for rollback.
- Mac: `com.fiorinis.<legacy>-*.plist.bak-2026-10-06-m3`,
  `~/.local/bin/<legacy>-zen-sync.bak-2026-10-06-m3`,
  `~/.codex/{config.toml,models_catalog.json}.bak-2026-10-06-m3`.

**Mini**
1. Worktree rsynced to `mini:/tmp/pb-build/` → `/opt/docker/provider-bridge/src/`
   (deploy source = worktree; nothing pushed to GitHub). Note: the worktree
   `.git` is a pointer, so `rsync --delete` removed `src/.git` — the deploy
   checkout is no longer a git repo (deviation; `update.sh`'s `git pull`
   needs a re-clone first; the worktree remains canonical).
2. `docker compose build` → new image `provider-bridge:latest` =
   `sha256:3c6cd2e05b00…`; binary `/app/providerbridge` (verified with `-help`).
3. `docker compose stop`. Clean stop checkpointed the WAL: `-wal`/`-shm` were
   **removed** (only `<legacy>.db`, 798720 B, remained).
4. `mv <legacy>.db provider-bridge.db` (no siblings to move). `config.yml`
   `db_sqlite.path` updated to `/app/data/provider-bridge.db` (required — the
   path is explicit, not the code default). Data ownership stayed `65532:65532`.
5. `config.yml` `user_agent: <legacy>-codex/1.0` → `provider-bridge-codex/1.0` (3 sites).
6. `docker-compose.yml`: only the healthcheck binary changed
   (`/app/<legacy>` → `/app/providerbridge`); service/container/image
   (`provider-bridge`) and volumes unchanged (keeps the affiora network alias).
7. `~/.<legacy>` and `.<legacy>.env`: **absent** (nothing to rename).
8. `deploy/mini/` (`PATCHES.md`, `update.sh`, `codex_regen.sh`,
   `MODEL-METADATA-RUNBOOK.md`, this doc) copied over the mini copies;
   scripts `0755`; both reference the new names.
9. `docker compose up -d` → healthy in ~15 s (image `3c6cd2e05b00`, entrypoint
   `/app/providerbridge`). Config graph intact: 3 providers, 10 models,
   11 routes, 10 offers.
10. `codex_regen.sh` → `model_provider = "provider-bridge"`,
    `[model_providers.provider-bridge]`; no stale names in `generated_configs/`.

**Verification (AGENTS.md §4.1 + M1 image row)**
| Row | Result |
|---|---|
| 1. Codex `/v1/responses` stream, tool + reasoning | PASS — `function_call` → round-trip → `response.completed` |
| 2. Claude Code `/v1/messages` non-stream + stream | PASS — thinking+text blocks; `message_start`/`content_block_start`/`message_stop` |
| 2. `claude -p … sonnet` / `haiku` / `opus` | PASS — all reply `OK` |
| 3. LibreChat (in-container, `host.docker.internal`) | PASS — non-stream `OK`; stream 22 chunks + `[DONE]` |
| 3. Affiora (in-container, `http://provider-bridge:38440`) | PASS — non-stream `OK`; stream 27 chunks + `[DONE]` |
| 3. Chat tool `arguments` type | PASS — JSON string `"{\"city\": \"Paris\"}"` |
| 4. `/v1/models` | PASS — `object:list`, `data[]` 10, `models` 21, 0 dupes |
| 5. Web search injection `/v1/messages` | PASS — tavily executed server-side, 0 leaked `tool_use`, fresh date answer |
| M1 image — multimodal (`mistral-medium-3.5`) | PASS — image forwarded natively, answered `green` |
| M1 image — text-only (`mistral-large-latest`) | PASS — orchestrator ran (`Visual tool executed tool=visual_brief images=1`), answered `green` |

`zai-glm-5-3` declined to invoke the injected `visual_brief` tool (model behaviour,
not a bridge defect); `mistral-large-latest` invoked it and answered correctly.

**Mac**
11. Unloaded + deleted `com.fiorinis.<legacy>-{tunnel,zen-sync}`; installed and
    loaded `com.fiorinis.provider-bridge-{tunnel,zen-sync}` (tunnel same
    `38440:127.0.0.1:38440` forward; logs renamed). Tunnel reachable again (`200`).
12. `~/.local/bin/<legacy>-zen-sync` → `provider-bridge-zen-sync`; constant
    `<LEGACY>` → `PROVIDER_BRIDGE`, provider key + `docker logs`/`ps` filter
    → `provider-bridge`, all `<legacy-name>`/`<legacy-binary>` strings replaced;
    `python3 -m py_compile` OK; 0 `moon` matches remain.
13. `~/.codex/config.toml` updated surgically (hand-maintained sections
    preserved): `model_provider` → `provider-bridge`,
    `[model_providers.provider-bridge]`, `name = "Provider Bridge"`.
    `models_catalog.json` needed no change — it contains zero old-name
    references and is byte-identical to the freshly regenerated catalog on mini.
    Codex app-server daemon restarted (`pkill -f codex.*app-server`).

**Post-cutover greps**
- mini live service files (`docker-compose.yml`, `config.yml`,
  `codex_regen.sh`, `update.sh`, `PATCHES.md`, `MODEL-METADATA-RUNBOOK.md`,
  `generated_configs/`): **0** `<legacy>` hits. The only remaining hits under
  `/opt/docker/provider-bridge` (excluding `src`) are this runbook (which
  documents the rename) and the `*.bak-*` backups.
- Mac live files (`~/.local/bin`, `~/Library/LaunchAgents`): **0** hits; only the
  `.bak-2026-10-06-m3` copies contain the old name.

**Rollback ready:** `docker tag provider-bridge:rollback-m3 provider-bridge:latest`,
restore `docker-compose.yml.bak-2026-10-06-m3`, restore `data/<legacy>.db*`
names, `docker compose up -d`. Not required — cutover green.
