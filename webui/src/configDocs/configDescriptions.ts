import type { Locale } from "../i18n/messages";

export type ConfigDocEntry = {
  path: string;
  title: Record<Locale, string>;
  description: Record<Locale, string>;
  type: string;
  defaultValue?: string;
  sensitive?: boolean;
  apply: Record<Locale, string>;
};

export const requiredConfigPaths = [
  "mode",
  "trace.enabled",
  "log.level",
  "log.format",
  "server.addr",
  "server.auth_token",
  "server.max_sessions",
  "server.session_ttl",
  "persistence.active_provider",
  "cache.mode",
  "cache.ttl",
  "cache.prompt_caching",
  "cache.automatic_prompt_cache",
  "cache.explicit_cache_breakpoints",
  "cache.allow_retention_downgrade",
  "cache.max_breakpoints",
  "cache.min_cache_tokens",
  "cache.expected_reuse",
  "cache.minimum_value_score",
  "cache.min_breakpoint_tokens",
  "defaults.model",
  "defaults.max_tokens",
  "defaults.system_prompt",
  "models.<slug>.context_window",
  "models.<slug>.max_output_tokens",
  "models.<slug>.slug",
  "models.<slug>.display_name",
  "models.<slug>.description",
  "models.<slug>.base_instructions",
  "models.<slug>.supports_reasoning",
  "models.<slug>.default_reasoning_level",
  "models.<slug>.supported_reasoning_levels",
  "models.<slug>.supports_reasoning_summaries",
  "models.<slug>.default_reasoning_summary",
  "models.<slug>.input_modalities",
  "models.<slug>.supports_image_detail_original",
  "models.<slug>.web_search",
  "models.<slug>.extensions",
  "providers.<key>.key",
  "providers.<key>.base_url",
  "providers.<key>.api_key",
  "providers.<key>.protocol",
  "providers.<key>.version",
  "providers.<key>.user_agent",
  "providers.<key>.web_search",
  "providers.<key>.extensions",
  "providers.<key>.offers[].model",
  "providers.<key>.offers[].upstream_name",
  "providers.<key>.offers[].priority",
  "providers.<key>.offers[].pricing",
  "providers.<key>.offers[].overrides",
  "routes.<alias>.alias",
  "routes.<alias>.to",
  "routes.<alias>.model",
  "routes.<alias>.provider",
  "routes.<alias>.display_name",
  "routes.<alias>.description",
  "routes.<alias>.context_window",
  "routes.<alias>.web_search",
  "routes.<alias>.extensions",
  "web_search.support",
  "web_search.max_uses",
  "web_search.tavily_api_key",
  "web_search.firecrawl_api_key",
  "web_search.search_max_rounds",
  "extensions.<name>.enabled",
  "extensions.<name>.config",
  "proxy.response",
  "proxy.anthropic"
] as const;

export type ConfigPath = (typeof requiredConfigPaths)[number];

export const configDescriptions: Record<ConfigPath, ConfigDocEntry> = {
  "mode": entry(
    "mode",
    "Run mode",
    "How Provider Bridge handles requests: convert between formats, or pass straight through to one provider.",
    "Transform | CaptureResponse | CaptureAnthropic",
    "Transform"
  ),
  "trace.enabled": entry(
    "trace.enabled",
    "Enable tracing",
    "Records how each request is handled, for troubleshooting.",
    "boolean"
  ),
  "log.level": entry(
    "log.level",
    "Log level",
    "Controls the minimum runtime log level.",
    "debug | info | warn | error",
    "info"
  ),
  "log.format": entry(
    "log.format",
    "Log format",
    "Controls whether runtime logs are emitted as text or JSON.",
    "text | json",
    "text"
  ),
  "server.addr": entry(
    "server.addr",
    "Listen address",
    "Address the server listens on. The console and API are served here.",
    "host:port",
    "127.0.0.1:38440"
  ),
  "server.auth_token": entry(
    "server.auth_token",
    "Auth token",
    "Password for the console and API. Leave empty to disable sign-in.",
    "string",
    "empty",
    true
  ),
  "server.max_sessions": entry(
    "server.max_sessions",
    "Max sessions",
    "Maximum retained session count; 0 means unlimited.",
    "number",
    "0"
  ),
  "server.session_ttl": entry(
    "server.session_ttl",
    "Session TTL",
    "How long session state is retained, for example 24h.",
    "string",
    "24h"
  ),
  "persistence.active_provider": entry(
    "persistence.active_provider",
    "Persistence provider",
    "Where settings are stored. Needed to save changes from the console.",
    "db_sqlite | db_d1",
    "db_sqlite"
  ),
  "cache.mode": entry(
    "cache.mode",
    "Cache mode",
    "How prompt caching works: off, explicit breakpoints, automatic, or a mix of both.",
    "off | explicit | automatic | hybrid",
    "explicit"
  ),
  "cache.ttl": entry(
    "cache.ttl",
    "Cache TTL",
    "Retention duration for cache entries, for example 1h.",
    "string"
  ),
  "cache.prompt_caching": entry(
    "cache.prompt_caching",
    "Enable prompt caching",
    "Allows Provider Bridge to enable prompt caching for supported upstream protocols.",
    "boolean"
  ),
  "cache.automatic_prompt_cache": entry(
    "cache.automatic_prompt_cache",
    "Automatic prompt cache",
    "Automatically chooses suitable cache breakpoints from request content.",
    "boolean"
  ),
  "cache.explicit_cache_breakpoints": entry(
    "cache.explicit_cache_breakpoints",
    "Explicit cache breakpoints",
    "Allows requests to explicitly specify prompt cache breakpoints.",
    "boolean"
  ),
  "cache.allow_retention_downgrade": entry(
    "cache.allow_retention_downgrade",
    "Allow retention downgrade",
    "Use a shorter cache lifetime if the provider doesn't support the chosen one.",
    "boolean"
  ),
  "cache.max_breakpoints": entry(
    "cache.max_breakpoints",
    "Max breakpoints",
    "Maximum number of cache breakpoints inserted for one request.",
    "number"
  ),
  "cache.min_cache_tokens": entry(
    "cache.min_cache_tokens",
    "Min cache tokens",
    "Segments below this token count are not considered cache candidates.",
    "number"
  ),
  "cache.expected_reuse": entry(
    "cache.expected_reuse",
    "Expected reuse",
    "How many times a cached block is expected to be reused (for automatic caching).",
    "number"
  ),
  "cache.minimum_value_score": entry(
    "cache.minimum_value_score",
    "Minimum value score",
    "Threshold below which automatic caching is skipped.",
    "number"
  ),
  "cache.min_breakpoint_tokens": entry(
    "cache.min_breakpoint_tokens",
    "Min breakpoint tokens",
    "Minimum number of tokens between two cache points.",
    "number"
  ),
  "defaults.model": entry(
    "defaults.model",
    "Default model",
    "Model used when a request doesn't specify one.",
    "string",
    "providerbridge"
  ),
  "defaults.max_tokens": entry(
    "defaults.max_tokens",
    "Default max tokens",
    "Default output limit when a request does not provide max_output_tokens.",
    "number",
    "65536"
  ),
  "defaults.system_prompt": entry(
    "defaults.system_prompt",
    "Global system prompt",
    "Global system prompt appended to requests, useful for behavior rules shared by all models.",
    "string",
    "empty"
  ),
  "models.<slug>.context_window": entry(
    "models.<slug>.context_window",
    "Context window",
    "Maximum tokens the model can handle at once.",
    "number"
  ),
  "models.<slug>.max_output_tokens": entry(
    "models.<slug>.max_output_tokens",
    "Max output tokens",
    "Maximum output tokens the model can emit in one response.",
    "number"
  ),
  "models.<slug>.slug": entry(
    "models.<slug>.slug",
    "Model slug",
    "Stable id other settings use to refer to this model.",
    "string"
  ),
  "models.<slug>.display_name": entry(
    "models.<slug>.display_name",
    "Model display name",
    "Human-readable model name shown in the console.",
    "string"
  ),
  "models.<slug>.description": entry(
    "models.<slug>.description",
    "Model description",
    "Describes model purpose, capabilities, or limits for console readers.",
    "string"
  ),
  "models.<slug>.base_instructions": entry(
    "models.<slug>.base_instructions",
    "Base instructions",
    "Default behavior instructions appended to requests for this model.",
    "string"
  ),
  "models.<slug>.supports_reasoning": entry(
    "models.<slug>.supports_reasoning",
    "Supports reasoning",
    "Marks whether this model supports reasoning configuration.",
    "boolean"
  ),
  "models.<slug>.default_reasoning_level": entry(
    "models.<slug>.default_reasoning_level",
    "Default reasoning level",
    "Default reasoning level used when a request does not specify one.",
    "string"
  ),
  "models.<slug>.supported_reasoning_levels": entry(
    "models.<slug>.supported_reasoning_levels",
    "Supported reasoning levels",
    "List of reasoning levels allowed for this model.",
    "array"
  ),
  "models.<slug>.supports_reasoning_summaries": entry(
    "models.<slug>.supports_reasoning_summaries",
    "Supports reasoning summaries",
    "Marks whether this model supports returning reasoning summaries.",
    "boolean"
  ),
  "models.<slug>.default_reasoning_summary": entry(
    "models.<slug>.default_reasoning_summary",
    "Default reasoning summary",
    "Default reasoning summary setting used when a request does not specify one.",
    "string"
  ),
  "models.<slug>.input_modalities": entry(
    "models.<slug>.input_modalities",
    "Input modalities",
    "Input types supported by this model, such as text or image.",
    "array"
  ),
  "models.<slug>.supports_image_detail_original": entry(
    "models.<slug>.supports_image_detail_original",
    "Supports original image detail",
    "Marks whether visual requests can send original image detail to this model.",
    "boolean"
  ),
  "models.<slug>.web_search": entry(
    "models.<slug>.web_search",
    "Model web search",
    "Overrides web search behavior for this model.",
    "object"
  ),
  "models.<slug>.extensions": entry(
    "models.<slug>.extensions",
    "Model extensions",
    "Overrides extension tools and config enabled for this model.",
    "object"
  ),
  "providers.<key>.key": entry(
    "providers.<key>.key",
    "Provider key",
    "Stable id routes use to pick this provider.",
    "string"
  ),
  "providers.<key>.base_url": entry(
    "providers.<key>.base_url",
    "Upstream base URL",
    "Upstream provider API URL.",
    "url"
  ),
  "providers.<key>.api_key": entry(
    "providers.<key>.api_key",
    "Upstream API key",
    "API key for this provider. Shown masked; enter a new value to update it.",
    "string",
    undefined,
    true
  ),
  "providers.<key>.protocol": entry(
    "providers.<key>.protocol",
    "Upstream protocol",
    "Selects the upstream API format: Anthropic Messages, OpenAI Responses, Google GenAI, or OpenAI Chat.",
    "anthropic | openai-response | google-genai | openai-chat",
    "anthropic"
  ),
  "providers.<key>.version": entry(
    "providers.<key>.version",
    "Protocol version",
    "API version header (mainly for Anthropic). Optional for some providers.",
    "string"
  ),
  "providers.<key>.user_agent": entry(
    "providers.<key>.user_agent",
    "User agent",
    "User-Agent string sent to the provider.",
    "string"
  ),
  "providers.<key>.web_search": entry(
    "providers.<key>.web_search",
    "Provider web search",
    "Overrides web search behavior for this provider.",
    "object"
  ),
  "providers.<key>.extensions": entry(
    "providers.<key>.extensions",
    "Provider extensions",
    "Overrides extension tools and config enabled for this provider.",
    "object"
  ),
  "providers.<key>.offers[].model": entry(
    "providers.<key>.offers[].model",
    "Provider model",
    "Which model this binding serves.",
    "string"
  ),
  "providers.<key>.offers[].upstream_name": entry(
    "providers.<key>.offers[].upstream_name",
    "Upstream model name",
    "Real model name sent to the provider. Leave empty to use the model id.",
    "string"
  ),
  "providers.<key>.offers[].priority": entry(
    "providers.<key>.offers[].priority",
    "Provider priority",
    "Tie-breaker when several providers serve the same model; lower wins.",
    "number"
  ),
  "providers.<key>.offers[].pricing": entry(
    "providers.<key>.offers[].pricing",
    "Billing",
    "Optional prices for cost tracking.",
    "number"
  ),
  "providers.<key>.offers[].overrides": entry(
    "providers.<key>.offers[].overrides",
    "Provider overrides",
    "Model capability overrides specific to this provider binding.",
    "object"
  ),
  "routes.<alias>.to": entry(
    "routes.<alias>.to",
    "Route target",
    "Internal target this route points to.",
    "string"
  ),
  "routes.<alias>.model": entry(
    "routes.<alias>.model",
    "Route model",
    "Model this alias points to.",
    "string"
  ),
  "routes.<alias>.alias": entry(
    "routes.<alias>.alias",
    "Route alias",
    "The model name clients send in requests.",
    "string"
  ),
  "routes.<alias>.provider": entry(
    "routes.<alias>.provider",
    "Route provider",
    "Provider key that handles this route.",
    "string"
  ),
  "routes.<alias>.display_name": entry(
    "routes.<alias>.display_name",
    "Route display name",
    "Human-readable route name shown in the console.",
    "string"
  ),
  "routes.<alias>.description": entry(
    "routes.<alias>.description",
    "Route description",
    "Describes this route's purpose or model selection policy.",
    "string"
  ),
  "routes.<alias>.context_window": entry(
    "routes.<alias>.context_window",
    "Route context window",
    "Context window limit exposed to clients for this route.",
    "number"
  ),
  "routes.<alias>.web_search": entry(
    "routes.<alias>.web_search",
    "Route web search",
    "Overrides web search behavior for this route.",
    "object"
  ),
  "routes.<alias>.extensions": entry(
    "routes.<alias>.extensions",
    "Route extensions",
    "Overrides extension tools and config enabled for this route.",
    "object"
  ),
  "web_search.support": entry(
    "web_search.support",
    "Web search mode",
    "auto prefers provider-native search and falls back to injection; enabled forces native; disabled turns it off; injected uses Tavily/Firecrawl tools.",
    "auto | enabled | disabled | injected",
    "auto"
  ),
  "web_search.max_uses": entry(
    "web_search.max_uses",
    "Max uses",
    "Limits how many web search calls one request may use.",
    "number"
  ),
  "web_search.tavily_api_key": entry(
    "web_search.tavily_api_key",
    "Tavily API key",
    "Tavily secret used by injected web search.",
    "string",
    undefined,
    true
  ),
  "web_search.firecrawl_api_key": entry(
    "web_search.firecrawl_api_key",
    "Firecrawl API key",
    "Firecrawl secret used by injected web search to fetch page content.",
    "string",
    undefined,
    true
  ),
  "web_search.search_max_rounds": entry(
    "web_search.search_max_rounds",
    "Search max rounds",
    "Maximum number of search rounds per request.",
    "number",
    "3"
  ),
  "extensions.<name>.enabled": entry(
    "extensions.<name>.enabled",
    "Enable extension",
    "Turn this extension on or off.",
    "boolean"
  ),
  "extensions.<name>.config": entry(
    "extensions.<name>.config",
    "Extension config",
    "Settings for this extension.",
    "object"
  ),
  "proxy.response": entry(
    "proxy.response",
    "OpenAI capture proxy",
    "Address, key, and default model used when passing requests straight through to OpenAI.",
    "object"
  ),
  "proxy.anthropic": entry(
    "proxy.anthropic",
    "Anthropic capture proxy",
    "Address, key, version, and model used when passing requests straight through to Anthropic.",
    "object"
  )
};

export function getConfigDescription(path: ConfigPath, locale: Locale) {
  const entry = configDescriptions[path];
  return {
    ...entry,
    title: entry.title[locale],
    description: entry.description[locale],
    apply: entry.apply[locale]
  };
}

function entry(
  path: ConfigPath,
  enTitle: string,
  enDescription: string,
  type: string,
  defaultValue?: string,
  sensitive = false
): ConfigDocEntry {
  return {
    path,
    title: { "en-US": enTitle },
    description: { "en-US": enDescription },
    type,
    defaultValue,
    sensitive,
    apply: {
      "en-US": "Saved instantly; some fields need a restart to take effect."
    }
  };
}
