package server

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"providerbridge/internal/config"
	"providerbridge/internal/extension/plugin"
	"providerbridge/internal/format"
	"providerbridge/internal/logger"
	"providerbridge/internal/protocol/chat"
	"providerbridge/internal/protocol/google"
	"providerbridge/internal/protocol/openai"
	"providerbridge/internal/service/api"
	"providerbridge/internal/service/provider"
	"providerbridge/internal/service/runtime"
	"providerbridge/internal/service/stats"
	"providerbridge/internal/service/store"
	"providerbridge/internal/service/webui"

	"providerbridge/internal/service/server/session"
	"providerbridge/internal/service/server/trace"
	"providerbridge/internal/service/server/usage"

	mbtrace "providerbridge/internal/service/trace"
)

// ChatClient is the interface for OpenAI-chat protocol clients.
// It uses any parameters to avoid importing protocol-specific packages.
type Config struct {
	// ServerCfg is the scoped domain config for the server layer.
	// Used alongside AppConfig for the full config.
	ServerCfg        config.ServerConfig
	AdapterRegistry  *format.Registry        // adapter dispatch path (format registry)
	Provider         provider.ProviderClient // fallback provider for non-adapter path
	ProviderMgr      *provider.ProviderManager
	OpenAIHTTPClient *http.Client
	ProxyHTTPClient  *http.Client
	ChatClients      map[string]any
	GoogleClients    map[string]any
	Tracer           *mbtrace.Tracer
	TraceErrors      io.Writer
	Stats            *stats.SessionStats
	PluginRegistry   *plugin.Registry
	AppConfig        config.ServerConfig
	Runtime          *runtime.Runtime
	Store            store.ConfigStore
	SessionManager   session.Manager
	UsageTracker     usage.Tracker
	TraceWriter      trace.Writer
}

type Server struct {
	adapterRegistry *format.Registry
	provider        provider.ProviderClient
	providerMgr     *provider.ProviderManager
	openAIHTTP      *http.Client
	proxyHTTP       *http.Client
	chatClients     map[string]any
	googleClients   map[string]any
	tracer          *mbtrace.Tracer
	traceErrors     io.Writer
	stats           *stats.SessionStats
	pluginRegistry  *plugin.Registry
	mux             *http.ServeMux
	onceClose       sync.Once
	appConfig       config.ServerConfig
	serverCfg       config.ServerConfig
	runtime         *runtime.Runtime
	store           store.ConfigStore
	sessionManager  session.Manager
	usageTracker    usage.Tracker
	traceWriter     trace.Writer

	// clientCaches holds lazily-created HTTP clients for runtime-reloaded providers.
	// Keyed by provider key, invalidated when Runtime reloads.
	clientCache   map[string]*chat.Client
	googleCache   map[string]*google.Client
	clientCacheMu sync.RWMutex
	googleCacheMu sync.RWMutex
	// clientCacheMgr is the manager the caches above were built from
	// (guarded by clientCacheMu). invalidateClientCacheOnManagerChange
	// clears the caches whenever the active manager differs.
	clientCacheMgr *provider.ProviderManager
}

// invalidateClientCacheOnManagerChange clears the lazily-built client caches
// whenever the active provider manager changed (e.g. a runtime Reload built
// a new manager), so a cached client never outlives the manager its API key
// came from. Safe to call on every cache read; a no-op when the manager is
// unchanged.
func (s *Server) invalidateClientCacheOnManagerChange() {
	pm := s.activeProviderManager()
	// Fast path: the manager is unchanged on the vast majority of calls
	// (this runs on every cache read, i.e. every chat request), so a
	// read lock suffices and the concurrent read path stays lock-free
	// with respect to other readers.
	s.clientCacheMu.RLock()
	if s.clientCacheMgr == pm {
		s.clientCacheMu.RUnlock()
		return
	}
	s.clientCacheMu.RUnlock()
	// Slow path: the manager changed — take the write lock and
	// DOUBLE-CHECK the mismatch before clearing: another goroutine may
	// have invalidated (and recorded this manager) in the meantime.
	s.clientCacheMu.Lock()
	defer s.clientCacheMu.Unlock()
	if s.clientCacheMgr == pm {
		return
	}
	s.clientCache = make(map[string]*chat.Client)
	s.clientCacheMgr = pm
	s.googleCacheMu.Lock()
	s.googleCache = make(map[string]*google.Client)
	s.googleCacheMu.Unlock()
}

func (s *Server) runtimeSnapshot() *runtime.ConfigSnapshot {
	if s.runtime == nil {
		return nil
	}
	return s.runtime.Current()
}

func (s *Server) activeProviderManager() *provider.ProviderManager {
	if snap := s.runtimeSnapshot(); snap != nil && snap.ProviderMgr != nil {
		return snap.ProviderMgr
	}
	return s.providerMgr
}

func (s *Server) activeProviderDefs() map[string]config.ProviderDef {
	if snap := s.runtimeSnapshot(); snap != nil {
		return snap.Config.ProviderDefs
	}
	return nil
}

func (s *Server) activeChatClient(providerKey string) any {
	s.invalidateClientCacheOnManagerChange()
	// Check runtime-driven cache first.
	s.clientCacheMu.RLock()
	if cached, ok := s.clientCache[providerKey]; ok {
		s.clientCacheMu.RUnlock()
		return cached
	}
	s.clientCacheMu.RUnlock()

	snap := s.runtimeSnapshot()
	if snap == nil {
		return s.chatClients[providerKey]
	}
	def, ok := snap.Config.ProviderDefs[providerKey]
	if !ok || def.Protocol != config.ProtocolOpenAIChat {
		return s.chatClients[providerKey]
	}
	// R6: def, API key, manager, and HTTP client all come from the SAME
	// snapshot — one consistent point-in-time view. Reading the def here
	// and the key from a SECOND live snapshot read (as this code once
	// did) lets a reload land between the reads and build a MISMATCHED
	// client (old BaseURL + new key) that the cache store below would
	// then accept (its live-manager check passes), disclosing the new
	// key to a retired endpoint.
	mgr := snap.ProviderMgr
	if mgr == nil {
		return s.chatClients[providerKey]
	}
	// Egress-proxy correctness: route through the manager's
	// proxy-aware client when one is configured.
	client := chat.NewClient(chat.ClientConfig{
		BaseURL:   def.BaseURL,
		APIKey:    mgr.ProviderAPIKey(providerKey),
		UserAgent: def.UserAgent,
		Client:    mgr.HTTPClient(providerKey),
	})
	// Cache only while mgr still owns the caches; when a reload swapped
	// the manager after this snapshot was taken, serve the built client
	// UNCACHED instead of nil: its key was active when the request
	// started (Reload preserves the rotation index), and a nil here
	// surfaces as a spurious 502 "no chat client for provider" under
	// live reload traffic.
	s.cacheChatClient(mgr, providerKey, client)
	return client
}

// cacheChatClient stores a freshly built chat client in the runtime
// client cache under cacheKey. mgr is the manager the client was built
// from — the origin of its API key. The store is atomic with the
// ownership check under clientCacheMu, and the check compares against
// the LIVE manager (s.activeProviderManager()), not the recorded
// s.clientCacheMgr: invalidation only runs on cache READS, so a reload
// landing between the mgr capture and this store leaves clientCacheMgr
// equal to the (now retired) mgr. Comparing against the live manager
// detects any reload in that window and refuses the store, so a
// retired-manager client never enters the cache — a reader whose own
// invalidation check already passed pre-reload could otherwise find it.
// Returns true when the client was cached; the caller serves the built
// client either way.
//
// Residual known race (P3, accepted): this live-manager check and the map
// store are atomic under clientCacheMu, but NOT with Runtime.Reload's
// lock-free snapshot swap — a reload landing in the few-instruction
// window between this check and a later invalidation can serve a
// pre-reload client for one read cycle. Since R6 every built client is a
// CONSISTENT (pre-reload key + endpoint) pair derived from a single
// snapshot, so a client that slips through can never mix generations;
// the next invalidation self-heals it. Do not restructure the runtime
// over this.
func (s *Server) cacheChatClient(mgr *provider.ProviderManager, cacheKey string, client *chat.Client) bool {
	s.clientCacheMu.Lock()
	defer s.clientCacheMu.Unlock()
	if s.activeProviderManager() != mgr {
		return false
	}
	s.clientCache[cacheKey] = client
	return true
}

// cacheGoogleClient is the google-genai counterpart of cacheChatClient:
// the ownership check compares against the LIVE manager (see the
// cacheChatClient comment for the mid-build reload window it closes),
// refusing a client whose key came from a retired manager. It acquires
// clientCacheMu before googleCacheMu — the same order as
// invalidateClientCacheOnManagerChange — so the ownership check and the
// store are atomic with respect to invalidation. Returns true when the
// client was cached; the caller serves the built client either way.
func (s *Server) cacheGoogleClient(mgr *provider.ProviderManager, providerKey string, client *google.Client) bool {
	s.clientCacheMu.Lock()
	defer s.clientCacheMu.Unlock()
	if s.activeProviderManager() != mgr {
		return false
	}
	s.googleCacheMu.Lock()
	defer s.googleCacheMu.Unlock()
	s.googleCache[providerKey] = client
	return true
}

// chatClientIndex returns the *chat.Client for the provider's API key at
// rotation index idx, building and caching it on first use. Cache entries
// are keyed "<provider>\x00<idx>" in s.clientCache.
func (s *Server) chatClientIndex(providerKey string, idx int) *chat.Client {
	s.invalidateClientCacheOnManagerChange()
	cacheKey := providerKey + "\x00" + strconv.Itoa(idx)
	s.clientCacheMu.RLock()
	if cached, ok := s.clientCache[cacheKey]; ok {
		s.clientCacheMu.RUnlock()
		return cached
	}
	s.clientCacheMu.RUnlock()
	// R6: key, manager, and def all come from the SAME snapshot; a
	// second live defs read after the key could pair a pre-reload key
	// with a post-reload BaseURL. See activeChatClient for the full
	// mismatch rationale.
	snap := s.runtimeSnapshot()
	if snap == nil {
		return nil
	}
	pm := snap.ProviderMgr
	if pm == nil {
		return nil
	}
	apiKey := pm.ProviderAPIKeyIndex(providerKey, idx)
	if apiKey == "" {
		return nil
	}
	def, ok := snap.Config.ProviderDefs[providerKey]
	if !ok || def.Protocol != config.ProtocolOpenAIChat {
		return nil
	}
	client := chat.NewClient(chat.ClientConfig{
		BaseURL:   def.BaseURL,
		APIKey:    apiKey,
		UserAgent: def.UserAgent,
		// Egress-proxy correctness: route through the manager's
		// proxy-aware client when one is configured.
		Client: pm.HTTPClient(providerKey),
	})
	// Cache only while pm still owns the caches; serve the built client
	// uncached when a reload swapped the manager mid-build (its key was
	// active when this request started — see cacheChatClient).
	s.cacheChatClient(pm, cacheKey, client)
	return client
}

// activeChatCaller returns the ChatCaller for providerKey: the rotating
// caller when the provider has multiple API keys, otherwise the active
// plain *chat.Client.
func (s *Server) activeChatCaller(providerKey string) provider.ChatCaller {
	pm := s.activeProviderManager()
	if pm != nil && pm.ProviderKeyCount(providerKey) > 1 {
		return provider.NewRotatingChatClient(pm, providerKey, func(idx int) *chat.Client {
			return s.chatClientIndex(providerKey, idx)
		})
	}
	if raw := s.activeChatClient(providerKey); raw != nil {
		if c, ok := raw.(*chat.Client); ok {
			return c
		}
	}
	return nil
}

func (s *Server) activeGoogleClient(providerKey string) any {
	s.invalidateClientCacheOnManagerChange()
	// Check runtime-driven cache first.
	s.googleCacheMu.RLock()
	if cached, ok := s.googleCache[providerKey]; ok {
		s.googleCacheMu.RUnlock()
		return cached
	}
	s.googleCacheMu.RUnlock()

	snap := s.runtimeSnapshot()
	if snap == nil {
		return s.googleClients[providerKey]
	}
	def, ok := snap.Config.ProviderDefs[providerKey]
	if !ok || def.Protocol != config.ProtocolGoogleGenAI {
		return s.googleClients[providerKey]
	}
	// R6: def and manager come from the SAME snapshot so a mid-build
	// reload cannot pair a pre-reload BaseURL with a post-reload key —
	// see activeChatClient for the mismatch rationale.
	mgr := snap.ProviderMgr
	if mgr == nil {
		// The snapshot has no manager (runtime booted without one) but
		// the server was booted with one: source the key from the boot
		// manager, matching activeProviderManager's fallback. Otherwise
		// def.APIKey — the raw comma-separated list — would leak every
		// key into the ?key= query param. Both-nil keeps def.APIKey,
		// identical to pre-R6 (test-only state).
		mgr = s.providerMgr
	}
	apiKey := def.APIKey
	if mgr != nil {
		// Google-genai clients are built from the ACTIVE API key. Google
		// rotation is explicitly out of scope (design D4): these clients
		// are cached per provider and never rotate on 429/402.
		apiKey = mgr.ProviderAPIKey(providerKey)
	}
	client := google.NewClient(google.ClientConfig{
		BaseURL:   def.BaseURL,
		APIKey:    apiKey,
		Project:   def.Project,
		Location:  def.Location,
		Version:   def.APIVersion,
		UserAgent: def.UserAgent,
	})
	// Same contract as activeChatClient: cache only while mgr still owns
	// the caches; serve the built client uncached when a reload swapped
	// the manager after this snapshot was taken.
	s.cacheGoogleClient(mgr, providerKey, client)
	return client
}

func New(cfg Config) *Server {
	if cfg.SessionManager == nil {
		cfg.SessionManager = newDefaultSessionManager(cfg)
	}
	s := &Server{
		adapterRegistry: cfg.AdapterRegistry,
		provider:        cfg.Provider,
		providerMgr:     cfg.ProviderMgr,
		openAIHTTP:      cfg.OpenAIHTTPClient,
		proxyHTTP:       cfg.ProxyHTTPClient,
		tracer:          cfg.Tracer,
		traceErrors:     cfg.TraceErrors,
		stats:           cfg.Stats,
		pluginRegistry:  cfg.PluginRegistry,
		mux:             http.NewServeMux(),
		appConfig:       cfg.AppConfig,
		serverCfg:       cfg.ServerCfg,
		chatClients:     cfg.ChatClients,
		googleClients:   cfg.GoogleClients,
		runtime:         cfg.Runtime,
		store:           cfg.Store,
		sessionManager:  cfg.SessionManager,
		usageTracker:    cfg.UsageTracker,
		traceWriter:     cfg.TraceWriter,
		clientCache:     make(map[string]*chat.Client),
		googleCache:     make(map[string]*google.Client),
	}
	s.mux.HandleFunc("/v1/responses", s.handleResponses)
	s.mux.HandleFunc("/responses", s.handleResponses)
	s.mux.HandleFunc("/v1/models", s.handleModels)
	s.mux.HandleFunc("/models", s.handleModels)
	s.mux.HandleFunc("/v1/messages", s.handleAnthropicMessages)
	s.mux.HandleFunc("/v1/messages/count_tokens", s.handleAnthropicCountTokens)
	s.mux.HandleFunc("/v1/chat/completions", s.handleChatCompletions)
	s.mux.Handle("/console/", webui.Embedded())
	s.registerPluginRoutes()
	if cfg.Runtime != nil && cfg.Store != nil {
		apiRouter := api.NewRouter(s.store, s.runtime, s.stats, s.pluginRegistry, s)
		s.mux.Handle("/api/v1/", http.StripPrefix("/api/v1", apiRouter))
	}
	return s
}

func (s *Server) ServeHTTP(writer http.ResponseWriter, request *http.Request) {
	if token := s.currentConfig().AuthToken; token != "" && !isConsoleAssetPath(request.URL.Path) {
		if !checkAuth(request, token) {
			writer.Header().Set("Content-Type", "application/json")
			writer.WriteHeader(http.StatusUnauthorized)
			json.NewEncoder(writer).Encode(openai.ErrorResponse{Error: openai.ErrorObject{
				Message: "missing or invalid bearer token; use the Authorization header with a Bearer scheme",
				Type:    "authentication_error",
				Code:    "invalid_auth",
			}})
			return
		}
	}
	s.mux.ServeHTTP(writer, request)
}

func isConsoleAssetPath(path string) bool {
	return path == "/console" || strings.HasPrefix(path, "/console/")
}

func (s *Server) handleModels(writer http.ResponseWriter, request *http.Request) {
	if request.Method != http.MethodGet {
		writeOpenAIError(writer, http.StatusMethodNotAllowed, openai.ErrorResponse{Error: openai.ErrorObject{
			Message: "only GET requests are supported",
			Type:    "invalid_request_error",
			Code:    "method_not_allowed",
		}})
		return
	}
	models := s.listModels()
	// Local patch 5: OpenAI-compatible /v1/models shape. The upstream
	// response only carries the proprietary "models" array, which breaks
	// OpenAI-shaped consumers (e.g. Open WebUI) that read data[].id from
	// {"object":"list","data":[...]}. Keep the legacy "models" key for the
	// embedded console and add the standard fields alongside it. The "id"
	// uses the provider/model slug, which ResolveModel accepts directly.
	//
	// The model list mixes provider/model entries with route aliases that
	// may point at the same upstream model (the short aliases exist for
	// Codex). Deduplicate by (provider, upstream model) so each model is
	// listed once; aliases are sorted by slug so the choice is stable.
	data := make([]map[string]any, 0, len(models))
	appendData := func(m map[string]any) {
		data = append(data, map[string]any{
			"id":       m["slug"],
			"object":   "model",
			"owned_by": m["provider"],
			"name":     m["name"],
		})
	}
	seen := make(map[string]bool, len(models))
	aliasEntries := make([]map[string]any, 0)
	for _, m := range models {
		if _, isAlias := m["model"]; isAlias {
			aliasEntries = append(aliasEntries, m)
			continue
		}
		seen[m["provider"].(string)+"/"+m["name"].(string)] = true
		appendData(m)
	}
	sort.Slice(aliasEntries, func(i, j int) bool {
		return aliasEntries[i]["slug"].(string) < aliasEntries[j]["slug"].(string)
	})
	for _, m := range aliasEntries {
		key := m["provider"].(string) + "/" + m["model"].(string)
		if seen[key] {
			continue
		}
		seen[key] = true
		appendData(m)
	}
	resp := struct {
		Object string           `json:"object"`
		Data   []map[string]any `json:"data"`
		Models []map[string]any `json:"models"`
	}{
		Object: "list",
		Data:   data,
		Models: models,
	}
	writer.Header().Set("Content-Type", "application/json")
	json.NewEncoder(writer).Encode(resp)
}

func (s *Server) listModels() []map[string]any {
	var models []map[string]any

	// Get provider data from runtime (full config snapshot).
	var providerDefs map[string]config.ProviderDef
	var routes map[string]config.RouteEntry
	if s.runtime != nil {
		fullCfg := s.runtime.Current().Config
		providerDefs = fullCfg.ProviderDefs
		routes = fullCfg.Routes
	}

	for key, def := range providerDefs {
		for modelName := range def.Models {
			models = append(models, map[string]any{
				"slug":     key + "/" + modelName,
				"name":     modelName,
				"provider": key,
			})
		}
	}

	for alias, route := range routes {
		displayName := route.DisplayName
		if displayName == "" {
			// When no explicit display_name is configured for this route,
			// derive from the alias slug (e.g. "gpt-5.4" -> "GPT 5.4").
			// This avoids inheriting the underlying model's DisplayName,
			// which would cause duplicates when multiple routes point to the same model.
			displayName = slugDisplayName(alias)
		}
		models = append(models, map[string]any{
			"slug":     alias,
			"name":     displayName,
			"provider": route.Provider,
			"model":    route.Model,
		})
	}
	return models
}

func (s *Server) currentConfig() config.ServerConfig {
	if snap := s.runtimeSnapshot(); snap != nil {
		return config.ServerFromGlobalConfig(&snap.Config)
	}
	return s.serverCfg
}

func (s *Server) CurrentConfig() api.ConfigAccessor {
	return s
}

func (s *Server) AuthToken() string {
	return s.currentConfig().AuthToken
}

func (s *Server) registerPluginRoutes() {
	if s.pluginRegistry == nil {
		return
	}
	s.pluginRegistry.RegisterRoutes(func(pattern string, handler http.Handler) {
		s.mux.Handle(pattern, handler)
	})
}

func InjectWebSearchTool(tools []openai.Tool) []openai.Tool {
	for _, t := range tools {
		if t.Type == "web_search" {
			return tools
		}
	}
	if tools == nil {
		tools = make([]openai.Tool, 0, 1)
	}
	return append(tools, openai.Tool{Type: "web_search"})
}

func (s *Server) Close() error {
	s.onceClose.Do(func() {
		if s.sessionManager != nil {
			s.sessionManager.Stop()
		}
	})
	return nil
}

func computeCostWithProviderPricing(pm *provider.ProviderManager, stats *stats.SessionStats, requestModel, actualModel, providerKey string, usage stats.BillingUsage) float64 {
	if stats == nil {
		return 0
	}
	if pm != nil {
		if meta, ok := pm.ModelMetaFor(actualModel, providerKey); ok {
			freshInput := float64(usage.FreshInputTokens)
			cacheWrite := float64(usage.CacheCreationInputTokens)
			cacheRead := float64(usage.CacheReadInputTokens)
			output := float64(usage.OutputTokens)
			cost := freshInput*meta.InputPrice/1000000 +
				cacheWrite*meta.CacheWritePrice/1000000 +
				cacheRead*meta.CacheReadPrice/1000000 +
				output*meta.OutputPrice/1000000
			if cost > 0 || meta.InputPrice > 0 || meta.OutputPrice > 0 {
				return cost
			}
		}
	}
	return stats.ComputeBillingCost(requestModel, usage)
}

// slugDisplayName converts a route alias slug to a human-readable display name.
// e.g. "gpt-5.4" -> "GPT 5.4", "codex-auto-review" -> "Codex Auto Review"
func slugDisplayName(slug string) string {
	slug = strings.ReplaceAll(slug, "-", " ")
	words := strings.Fields(slug)
	for i, w := range words {
		lower := strings.ToLower(w)
		if len(lower) >= 3 && lower[:3] == "gpt" {
			words[i] = "GPT" + w[3:]
			continue
		}
		words[i] = strings.ToUpper(w[:1]) + strings.ToLower(w[1:])
	}
	return strings.Join(words, " ")
}

func checkAuth(r *http.Request, expectedToken string) bool {
	auth := r.Header.Get("Authorization")
	if !strings.HasPrefix(auth, "Bearer ") {
		return false
	}
	return strings.TrimSpace(auth[7:]) == expectedToken
}

func (s *Server) resolveModelOrFallback(modelName string) (*provider.ResolvedRoute, error) {
	if pm := s.activeProviderManager(); pm != nil {
		return pm.ResolveModel(modelName)
	}
	if s.provider != nil {
		return &provider.ResolvedRoute{
			Candidates: []provider.ProviderCandidate{{
				ProviderKey:   "default",
				UpstreamModel: modelName,
				Protocol:      "anthropic",
				Client:        s.provider,
			}},
		}, nil
	}
	return nil, fmt.Errorf("no provider manager configured for model %q", modelName)
}

func requestHasImage(input json.RawMessage) bool {
	if len(input) == 0 || string(input) == "null" {
		return false
	}
	var items []struct {
		Type string `json:"type"`
	}
	if err := json.Unmarshal(input, &items); err == nil {
		for _, it := range items {
			switch it.Type {
			case "input_image", "image", "image_url":
				return true
			}
		}
		return false
	}
	return false
}

func (s *Server) filterCandidatesByInput(candidates []provider.ProviderCandidate, input json.RawMessage) ([]provider.ProviderCandidate, string) {
	pm := s.activeProviderManager()
	if pm == nil {
		return candidates, ""
	}
	hasImage := requestHasImage(input)
	if !hasImage {
		return candidates, ""
	}
	filtered := make([]provider.ProviderCandidate, 0, len(candidates))
	removedCount := 0
	for _, c := range candidates {
		meta, ok := pm.ModelMetaFor(c.UpstreamModel, c.ProviderKey)
		if !ok || !hasModalityImage(meta.InputModalities) {
			removedCount++
			logger.L().Debug("filtered image-incapable provider candidate", "provider", c.ProviderKey, "model", c.UpstreamModel)
			continue
		}
		filtered = append(filtered, c)
	}
	var reason string
	if removedCount > 0 {
		reason = fmt.Sprintf("request contains image input; filtered %d provider candidates without image support", removedCount)
	}
	return filtered, reason
}

func hasModalityImage(modalities []string) bool {
	for _, m := range modalities {
		if m == "image" {
			return true
		}
	}
	return false
}

// candidateSupportsImage reports whether the candidate's upstream model can
// natively consume image inputs. Missing metadata means NOT image-capable
// (mirrors filterCandidatesByInput), so such candidates get visual assist.
func (s *Server) candidateSupportsImage(c provider.ProviderCandidate) bool {
	pm := s.activeProviderManager()
	if pm == nil {
		return false
	}
	return pm.ModelSupportsImage(c.ProviderKey, c.UpstreamModel)
}

func newDefaultSessionManager(cfg Config) session.Manager {
	return session.NewInMemoryManager(&sessionConfigAdapter{runtime: cfg.Runtime, fallback: cfg.AppConfig}, cfg.PluginRegistry)
}

type sessionConfigAdapter struct {
	runtime  *runtime.Runtime
	fallback config.ServerConfig
}

func (a *sessionConfigAdapter) currentServerConfig() config.ServerConfig {
	if a.runtime != nil {
		snap := a.runtime.Current()
		if snap != nil {
			return config.ServerFromGlobalConfig(&snap.Config)
		}
	}
	return a.fallback
}

func (a *sessionConfigAdapter) SessionTTL() time.Duration {
	raw := a.currentServerConfig().SessionTTL
	if d, err := time.ParseDuration(raw); err == nil && d > 0 {
		return d
	}
	return 24 * time.Hour
}

func (a *sessionConfigAdapter) MaxSessions() int {
	return a.currentServerConfig().MaxSessions
}

func NewSessionConfigAdapter(cfg config.ServerConfig) session.ConfigAccessor {
	return &sessionConfigAdapter{fallback: cfg}
}

func NewSessionConfigAdapterFromRuntime(rt *runtime.Runtime, fallback config.ServerConfig) session.ConfigAccessor {
	return &sessionConfigAdapter{runtime: rt, fallback: fallback}
}
