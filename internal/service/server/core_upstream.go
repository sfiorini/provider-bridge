// Core-level upstream execution shared by the inbound protocol handlers
// (Anthropic Messages, Chat Completions). The OpenAI Responses inbound path
// keeps its own dispatch (handleWithAdapters/handleAdapterStream); this
// executor provides the same upstream semantics — provider adapter
// conversion, web-search injection loops, visual orchestration, DeepSeek
// reasoning replay — for inbounds that start from a CoreRequest.

package server

import (
	"context"
	"fmt"
	"log/slog"

	"providerbridge/internal/config"
	visualpkg "providerbridge/internal/extension/visual"
	"providerbridge/internal/extension/websearchinjected"
	"providerbridge/internal/format"
	"providerbridge/internal/protocol/anthropic"
	"providerbridge/internal/protocol/chat"
	"providerbridge/internal/protocol/openai"
	"providerbridge/internal/service/provider"
	"providerbridge/internal/session"
)

// coreUpstreamOutcome carries the result of upstream execution: either a
// complete CoreResponse (non-streaming) or a Core event stream (streaming).
type coreUpstreamOutcome struct {
	ModelAlias string
	Preferred  provider.ProviderCandidate
	Adapter    format.ProviderAdapter
	WSMode     string
	WSInjected bool
	CoreResp   *format.CoreResponse
	CoreEvents <-chan format.CoreStreamEvent
	// ProviderBuf returns the raw upstream stream events for post-stream
	// processing (trace capture, DeepSeek reasoning replay). Only set for
	// streaming requests.
	ProviderBuf func() []any
}

// injectCoreWebSearchAlias is the alias-only variant of injectCoreWebSearch
// for inbounds that are not the OpenAI Responses protocol. injectCoreWebSearch
// only reads the model alias from the request, so a stub suffices.
func (s *Server) injectCoreWebSearchAlias(ctx context.Context, coreReq *format.CoreRequest, preferred provider.ProviderCandidate, modelAlias string, wsMode string) bool {
	return s.injectCoreWebSearch(ctx, coreReq, preferred, openai.ResponsesRequest{Model: modelAlias}, wsMode)
}

// executeCoreUpstream dispatches a CoreRequest to the resolved upstream
// provider and returns the Core-level result. coreReq.Stream selects the
// streaming path.
//
// Supported upstream protocols: anthropic and openai-chat (the protocols
// behind all configured providers). Other protocols return an error that
// the calling handler renders in its own wire format.
func (s *Server) executeCoreUpstream(
	ctx context.Context,
	coreReq *format.CoreRequest,
	route *provider.ResolvedRoute,
	modelAlias string,
	sess *session.Session,
) (*coreUpstreamOutcome, error) {
	pm := s.activeProviderManager()

	preferred, ok := route.Preferred()
	if !ok {
		return nil, fmt.Errorf("no provider candidate for model %q", modelAlias)
	}

	switch preferred.Protocol {
	case config.ProtocolAnthropic, config.ProtocolOpenAIChat:
	default:
		return nil, fmt.Errorf("upstream protocol %q is not supported by this inbound endpoint", preferred.Protocol)
	}

	providerAdapter, ok := s.adapterRegistry.GetProvider(preferred.Protocol)
	if !ok {
		return nil, fmt.Errorf("no provider adapter for protocol %q", preferred.Protocol)
	}

	outcome := &coreUpstreamOutcome{ModelAlias: modelAlias, Preferred: preferred, Adapter: providerAdapter}

	// Override the Core model alias with the upstream model name.
	coreReq.Model = preferred.UpstreamModel

	outcome.WSMode = resolvedWebSearchMode(pm, modelAlias, preferred)
	outcome.WSInjected = s.injectCoreWebSearchAlias(ctx, coreReq, preferred, modelAlias, outcome.WSMode)
	searchCfg := s.resolvedSearchConfig(preferred.ProviderKey, modelAlias)

	upstreamAny, err := providerAdapter.FromCoreRequest(ctx, coreReq)
	if err != nil {
		return nil, fmt.Errorf("upstream conversion failed: %w", err)
	}

	switch preferred.Protocol {
	case config.ProtocolAnthropic:
		return s.executeAnthropicUpstream(ctx, coreReq, upstreamAny, outcome, searchCfg, sess)
	case config.ProtocolOpenAIChat:
		return s.executeChatUpstream(ctx, coreReq, upstreamAny, outcome, searchCfg, sess)
	default:
		return nil, fmt.Errorf("upstream protocol %q is not supported by this inbound endpoint", preferred.Protocol)
	}
}

// executeAnthropicUpstream runs the anthropic-protocol upstream half.
func (s *Server) executeAnthropicUpstream(
	ctx context.Context,
	coreReq *format.CoreRequest,
	upstreamAny any,
	outcome *coreUpstreamOutcome,
	searchCfg searchConfig,
	sess *session.Session,
) (*coreUpstreamOutcome, error) {
	anthReq, ok := upstreamAny.(*anthropic.MessageRequest)
	if !ok {
		return nil, fmt.Errorf("unexpected anthropic upstream request type %T", upstreamAny)
	}

	// Image-capability gate shared by both branches below: only requests
	// whose images the upstream cannot consume natively get visual assist
	// (or the strip fallback).
	hasImage := coreRequestHasImage(coreReq)
	supportsImg := s.candidateSupportsImage(outcome.Preferred)
	needsAssist := hasImage && !supportsImg
	visRan := false

	if outcome.WSMode == "enabled" {
		injectAnthropicWebSearch(anthReq)
	}
	if s.pluginRegistry != nil && sess != nil {
		prependCachedThinking(anthReq, sess)
	}

	finalizeAnthropic := func(_ context.Context, upstream any) (any, error) {
		msgReq, err := normalizeAnthropicRequest(upstream)
		if err != nil {
			return nil, err
		}
		if outcome.WSMode == "enabled" {
			injectAnthropicWebSearch(&msgReq)
		}
		if s.pluginRegistry != nil && sess != nil {
			prependCachedThinking(&msgReq, sess)
		}
		return &msgReq, nil
	}

	if coreReq.Stream {
		providerStream, ok := s.adapterRegistry.GetProviderStream(config.ProtocolAnthropic)
		if !ok {
			return nil, fmt.Errorf("anthropic stream adapter not available")
		}

		// Visual orchestrator: for image inputs the upstream cannot consume
		// natively, run non-streaming orchestration and synthesize a Core
		// stream.
		if needsAssist && s.pluginRegistry != nil && s.runtime != nil {
			if visProv := s.wrapWithVisual(ctx, outcome.ModelAlias, outcome.Preferred, outcome.Adapter, finalizeAnthropic); visProv != nil {
				visRan = true
				coreResp, err := visProv.CreateCore(ctx, coreReq)
				if err != nil {
					return nil, fmt.Errorf("visual orchestration failed: %w", err)
				}
				if outcome.WSInjected {
					coreResp, err = executeCoreSearchLoop(ctx, visProv, coreReq, coreResp, searchCfg.tavilyKey, searchCfg.firecrawlKey, searchCfg.maxRounds)
					if err != nil {
						return nil, fmt.Errorf("visual search orchestration failed: %w", err)
					}
				}
				outcome.CoreEvents = coreResponseToCoreStream(ctx, coreResp)
				return outcome, nil
			}
		}

		effectiveProvider := outcome.Preferred.Client
		if effectiveProvider == nil {
			return nil, fmt.Errorf("no upstream provider for model %q", coreReq.Model)
		}
		typedClient, err := provider.AsAnthropicUpstream(effectiveProvider)
		if err != nil {
			return nil, fmt.Errorf("provider %q does not support anthropic streaming", outcome.Preferred.ProviderKey)
		}

		if needsAssist && !visRan {
			slog.Default().Warn("visual assist unavailable; stripping images before forwarding to a text-only upstream",
				"provider", outcome.Preferred.ProviderKey, "model", outcome.Preferred.UpstreamModel)
			stripped, _ := visualpkg.StripImagesFromAnthropic(*anthReq)
			anthReq = &stripped
		}

		stream, err := typedClient.StreamMessage(ctx, *anthReq)
		if err != nil {
			return nil, fmt.Errorf("upstream stream error: %w", err)
		}
		sr, err := providerToCoreStream(ctx, providerStream, coreReq, stream)
		if err != nil {
			return nil, fmt.Errorf("anthropic stream conversion failed: %w", err)
		}
		outcome.CoreEvents = sr.Events
		outcome.ProviderBuf = sr.StreamBuffer
		return outcome, nil
	}

	// Non-streaming.
	effectiveProvider := outcome.Preferred.Client
	if effectiveProvider == nil {
		return nil, fmt.Errorf("no upstream provider for model %q", coreReq.Model)
	}
	if outcome.WSInjected {
		if typedClient, terr := provider.AsAnthropicUpstream(effectiveProvider); terr == nil {
			wrapped := websearchinjected.WrapProvider(typedClient, searchCfg.tavilyKey, searchCfg.firecrawlKey, searchCfg.maxRounds, s.proxyHTTP)
			effectiveProvider = &searchProviderAdapter{wrapped: wrapped}
		}
	}

	if needsAssist {
		if visProv := s.wrapWithVisual(ctx, outcome.ModelAlias, outcome.Preferred, outcome.Adapter, finalizeAnthropic); visProv != nil {
			visRan = true
			coreResp, err := visProv.CreateCore(ctx, coreReq)
			if err != nil {
				return nil, fmt.Errorf("visual orchestration failed: %w", err)
			}
			if outcome.WSInjected {
				coreResp, err = executeCoreSearchLoop(ctx, visProv, coreReq, coreResp, searchCfg.tavilyKey, searchCfg.firecrawlKey, searchCfg.maxRounds)
				if err != nil {
					return nil, fmt.Errorf("visual search orchestration failed: %w", err)
				}
			}
			outcome.CoreResp = coreResp
			return outcome, nil
		}
	}

	if needsAssist && !visRan {
		slog.Default().Warn("visual assist unavailable; stripping images before forwarding to a text-only upstream",
			"provider", outcome.Preferred.ProviderKey, "model", outcome.Preferred.UpstreamModel)
		stripped, _ := visualpkg.StripImagesFromAnthropic(*anthReq)
		anthReq = &stripped
	}

	var msgResp anthropic.MessageResponse
	if outcome.WSInjected {
		// WS-injected: effectiveProvider is the searchProviderAdapter
		// wrapping the orchestrator, which already holds the rotating
		// typed client and rotates internally.
		rawResp, err := effectiveProvider.CreateMessage(ctx, *anthReq)
		if err != nil {
			return nil, fmt.Errorf("upstream error: %w", err)
		}
		resp, ok := rawResp.(anthropic.MessageResponse)
		if !ok {
			return nil, fmt.Errorf("unexpected anthropic response type %T", rawResp)
		}
		msgResp = resp
	} else {
		typedClient, err := provider.AsAnthropicUpstream(effectiveProvider)
		if err != nil {
			return nil, fmt.Errorf("provider %q does not support anthropic upstream calls", outcome.Preferred.ProviderKey)
		}
		msgResp, err = typedClient.CreateMessage(ctx, *anthReq)
		if err != nil {
			return nil, fmt.Errorf("upstream error: %w", err)
		}
	}
	coreResp, err := providerToCoreResponse(ctx, outcome.Adapter, coreReq, &msgResp)
	if err != nil {
		return nil, fmt.Errorf("response conversion failed: %w", err)
	}
	outcome.CoreResp = coreResp
	return outcome, nil
}

// executeChatUpstream runs the openai-chat-protocol upstream half.
func (s *Server) executeChatUpstream(
	ctx context.Context,
	coreReq *format.CoreRequest,
	upstreamAny any,
	outcome *coreUpstreamOutcome,
	searchCfg searchConfig,
	sess *session.Session,
) (*coreUpstreamOutcome, error) {
	chatReq, ok := upstreamAny.(*chat.ChatRequest)
	if !ok {
		return nil, fmt.Errorf("unexpected chat upstream request type %T", upstreamAny)
	}

	if s.pluginRegistry != nil && sess != nil {
		prependCachedReasoningForChat(chatReq, sess, outcome.Preferred.ProviderKey == "deepseek")
	}

	finalizeChat := func(_ context.Context, upstream any) (any, error) {
		req, ok := upstream.(*chat.ChatRequest)
		if !ok {
			return nil, fmt.Errorf("finalizeChat: expected *chat.ChatRequest, got %T", upstream)
		}
		if s.pluginRegistry != nil && sess != nil {
			prependCachedReasoningForChat(req, sess, outcome.Preferred.ProviderKey == "deepseek")
		}
		return req, nil
	}

	chatClientRaw := s.activeChatClient(outcome.Preferred.ProviderKey)
	if chatClientRaw == nil {
		return nil, fmt.Errorf("no chat client for provider %q", outcome.Preferred.ProviderKey)
	}
	chatClient, ok := chatClientRaw.(*chat.Client)
	if !ok {
		return nil, fmt.Errorf("invalid chat client for provider %q", outcome.Preferred.ProviderKey)
	}

	// Non-streaming search loop / streaming buffered search both need the
	// visual candidate with the chat client attached.
	visualCandidate := outcome.Preferred
	visualCandidate.Client = &chatProviderClient{c: chatClient}

	// Image-capability gate shared by both branches below: only requests
	// whose images the upstream cannot consume natively get visual assist
	// (or the strip fallback).
	hasImage := coreRequestHasImage(coreReq)
	supportsImg := s.candidateSupportsImage(outcome.Preferred)
	needsAssist := hasImage && !supportsImg
	visRan := false

	if coreReq.Stream {
		providerStream, ok := s.adapterRegistry.GetProviderStream(config.ProtocolOpenAIChat)
		if !ok {
			return nil, fmt.Errorf("chat stream adapter not available")
		}

		// Visual orchestrator path.
		if needsAssist && s.pluginRegistry != nil && s.runtime != nil {
			if visProv := s.wrapWithVisual(ctx, outcome.ModelAlias, visualCandidate, outcome.Adapter, finalizeChat); visProv != nil {
				visRan = true
				coreResp, err := visProv.CreateCore(ctx, coreReq)
				if err != nil {
					return nil, fmt.Errorf("chat visual orchestration failed: %w", err)
				}
				if outcome.WSInjected {
					coreResp, err = executeCoreSearchLoop(ctx, visProv, coreReq, coreResp, searchCfg.tavilyKey, searchCfg.firecrawlKey, searchCfg.maxRounds)
					if err != nil {
						return nil, fmt.Errorf("visual search orchestration failed: %w", err)
					}
				}
				outcome.CoreEvents = coreResponseToCoreStream(ctx, coreResp)
				return outcome, nil
			}
		}

		if needsAssist && !visRan {
			slog.Default().Warn("visual assist unavailable; stripping images before forwarding to a text-only upstream",
				"provider", outcome.Preferred.ProviderKey, "model", outcome.Preferred.UpstreamModel)
			strippedReq, _ := visualpkg.StripImagesFromChat(*chatReq)
			chatReq = &strippedReq
		}

		var chatStream <-chan chat.ChatStreamChunk
		var err error
		if outcome.WSInjected {
			chatStream, err = s.chatSearchBufferedStream(ctx, chatClient, chatReq, searchCfg.tavilyKey, searchCfg.firecrawlKey, searchCfg.maxRounds)
		} else {
			chatStream, err = chatClient.StreamChat(ctx, chatReq)
		}
		if err != nil {
			return nil, fmt.Errorf("chat stream error: %w", err)
		}
		sr, err := providerToCoreStream(ctx, providerStream, coreReq, chatStream)
		if err != nil {
			return nil, fmt.Errorf("chat stream conversion failed: %w", err)
		}
		outcome.CoreEvents = sr.Events
		outcome.ProviderBuf = sr.StreamBuffer
		return outcome, nil
	}

	// Non-streaming.
	if needsAssist {
		if visProv := s.wrapWithVisual(ctx, outcome.ModelAlias, visualCandidate, outcome.Adapter, finalizeChat); visProv != nil {
			visRan = true
			coreResp, err := visProv.CreateCore(ctx, coreReq)
			if err != nil {
				return nil, fmt.Errorf("chat visual orchestration failed: %w", err)
			}
			if outcome.WSInjected {
				coreResp, err = executeCoreSearchLoop(ctx, visProv, coreReq, coreResp, searchCfg.tavilyKey, searchCfg.firecrawlKey, searchCfg.maxRounds)
				if err != nil {
					return nil, fmt.Errorf("visual search orchestration failed: %w", err)
				}
			}
			outcome.CoreResp = coreResp
			return outcome, nil
		}
	}

	if needsAssist && !visRan {
		slog.Default().Warn("visual assist unavailable; stripping images before forwarding to a text-only upstream",
			"provider", outcome.Preferred.ProviderKey, "model", outcome.Preferred.UpstreamModel)
		strippedReq, _ := visualpkg.StripImagesFromChat(*chatReq)
		chatReq = &strippedReq
	}

	var chatResp *chat.ChatResponse
	var err error
	if outcome.WSInjected {
		chatResp, err = s.executeChatSearchLoop(ctx, chatClient, chatReq, searchCfg.tavilyKey, searchCfg.firecrawlKey, searchCfg.maxRounds)
	} else {
		chatResp, err = chatClient.CreateChat(ctx, chatReq)
	}
	if err != nil {
		return nil, fmt.Errorf("chat upstream error: %w", err)
	}
	coreResp, err := providerToCoreResponse(ctx, outcome.Adapter, coreReq, chatResp)
	if err != nil {
		return nil, fmt.Errorf("chat response conversion failed: %w", err)
	}
	outcome.CoreResp = coreResp

	// Cache reasoning for DeepSeek thinking replay.
	if sess != nil {
		for _, choice := range chatResp.Choices {
			if choice.Message.ReasoningContent != "" && len(choice.Message.ToolCalls) > 0 {
				var tcIDs []string
				for _, tc := range choice.Message.ToolCalls {
					tcIDs = append(tcIDs, tc.ID)
				}
				cacheReasoningForChat(sess, tcIDs, choice.Message.ReasoningContent)
			}
		}
	}

	return outcome, nil
}
