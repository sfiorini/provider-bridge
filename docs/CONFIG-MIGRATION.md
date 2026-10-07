# Config Migration

Provider Bridge is pre-release: config changes switch directly to the current
format with no runtime aliases for old fields. Migrate an old config with the
one-shot script, then maintain it in the new shape.

There are two scripts in `scripts/`:

- `migrate_config_v5.py` — pre-v5 → v5 (the current top-level
  `providers`/`models`/`routes` layout).
- `migrate_config.py` — the older pre-`provider/routes` → current
  provider/routes format (per-provider `models` keyed by upstream model name,
  the `extensions` slot).

Both are `uv` scripts (PEP 723) requiring Python ≥3.10 and `ruamel.yaml`.

## v4 → v5 migration (current format)

Moves from v4 (the `provider.providers` nesting) to v5 (top-level
`providers` / `models` / `routes`).

### Usage

```bash
uv run scripts/migrate_config_v5.py config.yml output.yml
```

The script takes exactly two positional arguments (input, output) and writes a
new file. **There is no `--dry-run` flag.** To preview, copy your config first
and run the migration against the copy:

```bash
cp config.yml config.migrated-preview.yml
uv run scripts/migrate_config_v5.py config.migrated-preview.yml config.out.yml
diff config.yml config.out.yml
```

### What changes

| Old (v4) | New (v5) |
|----------|----------|
| `provider.providers.<key>.models` (client-alias mapping) | Shared metadata moves to top-level `models.<slug>`; the provider declares `providers.<key>.offers[].model` |
| `routes[].to` (e.g. `"deepseek/deepseek-v4-pro"`) | `routes[].model` + `routes[].provider` |
| `provider.base_url` / `provider.api_key` (top level) | Removed; use `providers.<key>.base_url` / `api_key` |
| `provider.default_model` / `provider.default_max_tokens` / `system_prompt` | `defaults.model` / `defaults.max_tokens` / `defaults.system_prompt` |
| `trace_requests: true` | `trace: { enabled: true }` |
| `developer.proxy.*` | `proxy.*` |

### Example

v4:

```yaml
provider:
  providers:
    deepseek:
      base_url: "https://api.deepseek.com/anthropic"
      api_key: "sk-xxx"
      models:
        deepseek-v4-pro:
          extensions:
            deepseek_v4:
              enabled: true
  routes:
    providerbridge:
      to: "deepseek/deepseek-v4-pro"
```

After migration (v5):

```yaml
providers:
  deepseek:
    base_url: "https://api.deepseek.com/anthropic"
    api_key: "sk-xxx"
    offers:
      - model: deepseek-v4-pro

models:
  deepseek-v4-pro:
    extensions:
      deepseek_v4:
        enabled: true

routes:
  providerbridge:
    model: deepseek-v4-pro
    provider: deepseek

defaults:
  model: deepseek-v4-pro
  max_tokens: 4096
```

### Caveats

- A shared model slug must be unique across the config. If several providers
  offer the same slug, reference it repeatedly in each provider's `offers`.
- Pricing moves from the model definition to `offers[].pricing`, so it is
  configured per provider.
- Provider-level `web_search` / `extensions` settings are preserved on the
  provider definition.
- Back up the original config before running the migration.
