#!/usr/bin/env -S uv run --script
# /// script
# requires-python = ">=3.10"
# dependencies = ["ruamel.yaml"]
# ///
"""Migrate ProviderBridge config.yml from pre-v5 format to v5 format.

The v5 format separates models from providers and places them at the top level.

Key changes:
  1. `provider.providers` → top-level `providers`
  2. `provider.models` metadata → top-level `models.<slug>`
  3. `provider.default_model` / `provider.default_max_tokens` / `system_prompt` → `defaults.*`
  4. `trace_requests: true` → `trace: { enabled: true }`
  5. `developer.proxy.*` → `proxy.*`
  6. `routes[].to` → `routes[].model` + `routes[].provider`
  7. `provider.base_url`, `provider.api_key`, `provider.version`, `provider.user_agent` removed
  8. Provider `models` converted to `offers` + top-level `models`.`slug`

Usage:
  python3 scripts/migrate_config_v5.py input.yml output.yml
"""

import sys
import warnings
from pathlib import Path

try:
    from ruamel.yaml import YAML
except ImportError:
    print("ruamel.yaml is required. Install with: pip install ruamel.yaml", file=sys.stderr)
    sys.exit(1)


def migrate(input_path: str, output_path: str) -> None:
    yaml = YAML()
    yaml.preserve_quotes = True
    yaml.indent(mapping=2, sequence=4, offset=2)

    with open(input_path, "r") as f:
        data = yaml.load(f)

    if data is None:
        data = {}

    # 1. Extract old provider block and legacy fields.
    old_provider = data.pop("provider", {}) or {}
    old_system_prompt = data.pop("system_prompt", None)
    old_developer = data.pop("developer", {}) or {}

    # 2. Top-level models (extracted from provider.providers[*].models).
    top_models = data.get("models", {}) or {}
    top_providers = data.get("providers", {}) or {}
    top_routes = data.get("routes", None) or old_provider.pop("routes", None) or {}

    nested_providers = old_provider.pop("providers", {}) or {}

    # Merge: new-providers take precedence, old ones are added.
    for key, value in nested_providers.items():
        if key not in top_providers:
            top_providers[key] = value

    # 3. Extract models from each provider's models section.
    for pkey, pdef in list(top_providers.items()):
        provider_models = pdef.pop("models", None) or {}
        offers = pdef.get("offers", []) or []

        for slug, mdef in provider_models.items():
            # Extract pricing from model metadata.
            pricing = mdef.pop("pricing", None) or {}
            # Remove metadata fields that go into top-level models.
            offer_entry = {"model": slug}
            if pricing:
                offer_entry["pricing"] = pricing

            # Check for upstream name (if mdef has "name" field).
            upstream_name = mdef.pop("name", None)
            if upstream_name:
                offer_entry["upstream_name"] = upstream_name

            offers.append(offer_entry)

            # Remaining metadata goes into top-level models if not already defined.
            if slug not in top_models:
                top_model = {}
                # Copy over known metadata fields.
                for field in [
                    "context_window", "max_output_tokens", "display_name",
                    "description", "base_instructions", "default_reasoning_level",
                    "supported_reasoning_levels", "supports_reasoning_summaries",
                    "default_reasoning_summary", "input_modalities",
                    "supports_image_detail_original",
                ]:
                    if field in mdef:
                        top_model[field] = mdef.pop(field)
                # WebSearch and Extensions.
                if "web_search" in mdef:
                    top_model["web_search"] = mdef.pop("web_search")
                if "extensions" in mdef:
                    top_model["extensions"] = mdef.pop("extensions")
                # Any remaining unknown fields.
                leftovers = {k: v for k, v in mdef.items() if k not in ("pricing", "name")}
                if leftovers:
                    top_model.update(leftovers)
                if top_model:
                    top_models[slug] = top_model

            # Handle duplicate slugs: merge offers, warn.
            if slug in top_models and any(
                o.get("model") == slug for o in offers[:-1]
            ):
                warnings.warn(
                    f"Duplicate model slug {slug!r} in provider {pkey!r}; "
                    f"adding as additional offer"
                )

        # Migrate provider-level priority to each offer.
        provider_priority = pdef.pop("priority", None)
        if provider_priority is not None:
            for offer_entry in offers:
                if "priority" not in offer_entry:
                    offer_entry["priority"] = provider_priority

        if offers:
            pdef["offers"] = offers
        else:
            pdef.pop("offers", None)

    # Warn about remaining unrecognized fields in provider defs.
    for pkey, pdef in list(top_providers.items()):
        for field in list(pdef.keys()):
            if field in (
                "base_url", "api_key", "version", "user_agent", "protocol",
                "web_search", "extensions", "offers",
            ):
                continue
            warnings.warn(
                f"Unrecognized field {field!r} in provider {pkey!r}; "
                f"it will be kept but may not be valid in v5"
            )

    # 4. Extract defaults.
    defaults = data.get("defaults", None) or {}
    if old_system_prompt is not None and "system_prompt" not in defaults:
        defaults["system_prompt"] = old_system_prompt
    old_default_model = old_provider.pop("default_model", None)
    if old_default_model is not None and "model" not in defaults:
        defaults["model"] = old_default_model
    old_default_max_tokens = old_provider.pop("default_max_tokens", None)
    if old_default_max_tokens is not None and "max_tokens" not in defaults:
        defaults["max_tokens"] = old_default_max_tokens
    if defaults:
        data["defaults"] = defaults

    # 5. Trace.
    old_trace = data.pop("trace_requests", None)
    if old_trace is not None:
        trace = data.get("trace", None) or {}
        if "enabled" not in trace:
            trace["enabled"] = old_trace
        data["trace"] = trace

    # 6. Web search: extract from old provider.web_search to top-level.
    old_web_search = old_provider.pop("web_search", None) or {}
    top_web_search = data.get("web_search", None) or {}
    for key in ("support", "max_uses", "tavily_api_key", "firecrawl_api_key", "search_max_rounds"):
        if key in old_web_search and key not in top_web_search:
            top_web_search[key] = old_web_search[key]
    if top_web_search:
        data["web_search"] = top_web_search

    # 7. Remove old top-level provider fields.
    for legacy_field in ("base_url", "api_key", "version", "user_agent"):
        old_provider.pop(legacy_field, None)

    # Clear provider section if empty.
    if old_provider:
        # Keep any remaining fields but warn.
        warnings.warn(f"Remaining fields in old provider block: {list(old_provider.keys())}")

    # 8. Proxy: migrate from developer.proxy to top-level proxy.
    old_proxy = old_developer.pop("proxy", None) or {}
    top_proxy = data.get("proxy", None) or {}
    for proxy_key in ("response", "anthropic"):
        old_target = old_proxy.get(proxy_key, None) or {}
        new_target = top_proxy.get(proxy_key, None) or {}
        # Flatten old nested "provider" block.
        old_provider_block = old_target.pop("provider", None) or {}
        for field in ("base_url", "api_key", "version"):
            if field in old_provider_block and field not in new_target:
                new_target[field] = old_provider_block[field]
        if old_target.get("model") and "model" not in new_target:
            new_target["model"] = old_target["model"]
        if new_target:
            top_proxy[proxy_key] = new_target
    if top_proxy:
        data["proxy"] = top_proxy

    # 9. Routes: convert "to" format to model+provider.
    for alias, route in list(top_routes.items()):
        if isinstance(route, str):
            # Old format: "provider/model"
            if "/" in route:
                provider, model = route.split("/", 1)
                top_routes[alias] = {"model": model.strip(), "provider": provider.strip()}
            else:
                top_routes[alias] = {"model": route.strip()}
        elif isinstance(route, dict):
            to_val = route.pop("to", None)
            if to_val and "model" not in route:
                if "/" in to_val:
                    provider, model = to_val.split("/", 1)
                    route["model"] = model.strip()
                    route["provider"] = provider.strip()
                else:
                    route["model"] = to_val.strip()

    # 10. Ensure all route model slugs have top-level model entries.
    for alias, route in list(top_routes.items()):
        if isinstance(route, dict):
            model_slug = route.get("model", "")
            if model_slug and model_slug not in top_models:
                top_models[model_slug] = {
                    "display_name": model_slug,
                    "description": f"Model referenced by route {alias}",
                }
                warnings.warn(
                    f"Route {alias!r} references model {model_slug!r} without a provider model definition; "
                    f"created minimal top-level model entry"
                )

    # Write output.
    if top_models:
        data["models"] = top_models
    if top_providers:
        data["providers"] = top_providers
    if top_routes:
        data["routes"] = top_routes

    # Clean up empty sections.
    for key in ("models", "providers", "routes", "extensions", "defaults", "trace", "web_search", "proxy", "cache", "persistence", "log", "server"):
        if key in data and not data[key]:
            del data[key]

    with open(output_path, "w") as f:
        yaml.dump(data, f)

    print(f"Migrated {input_path} -> {output_path}")


if __name__ == "__main__":
    if len(sys.argv) != 3:
        print(f"Usage: {sys.argv[0]} input.yml output.yml", file=sys.stderr)
        sys.exit(1)
    migrate(sys.argv[1], sys.argv[2])
