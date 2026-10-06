# 配置迁移

Provider Bridge 还没有公开发布，配置结构变更时会直接切到当前格式，不在运行时保留旧字段别名。旧配置请用迁移脚本做一次性迁移，然后按新结构维护。

---

## v5 迁移（当前格式）

从 v4（含 `provider.providers` 嵌套格式）迁移到 v5（顶层 `providers`/`models`/`routes`）。

迁移脚本：`scripts/migrate_config_v5.py`

### 使用方式

```bash
uv run scripts/migrate_config_v5.py config.yml output.yml
```

建议先跑 `--dry-run` 预览结果（脚本暂不支持 dry-run 标志，可先复制配置做测试）。

### 主要变更

| 旧格式 (v4) | 新格式 (v5) |
|---|---|
| `provider.providers.<key>.models`（客户端别名映射） | 共享模型元数据放顶层 `models.<slug>`，提供商声明放 `providers.<key>.offers[].model` |
| `routes[].to`（如 `"deepseek/deepseek-v4-pro"`） | `routes[].model` + `routes[].provider` |
| `provider.base_url` / `provider.api_key`（顶层） | 删除，改为 `providers.<key>.base_url` / `api_key` |
| `provider.default_model` / `provider.default_max_tokens` / `system_prompt` | `defaults.model` / `defaults.max_tokens` / `defaults.system_prompt` |
| `trace_requests: true` | `trace: { enabled: true }` |
| `developer.proxy.*` | `proxy.*` |

### 示例

v4 格式：

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

迁移后 (v5)：

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

### 注意事项

- 共享模型 slug 在整个配置中必须唯一。如果多个 provider 提供同一个 slug，在 `offers` 中重复引用即可。
- 定价从模型定义层迁移到 `offers[].pricing`，按 provider 分别设置。
- 旧格式中 provider 级的 `web_search` / `extensions` 配置会保留在 provider 定义中。
- 运行迁移脚本前建议备份原配置。

