// Inbound HTTP handlers for the Anthropic Messages and OpenAI Chat
// Completions endpoints. Both convert their wire requests to Core early and
// share the Core-level upstream executor (executeCoreUpstream), inheriting
// model alias/slug resolution, web-search injection, visual orchestration,
// DeepSeek reasoning replay, sessions, tracing, and usage stats.

package server

import (
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"time"

	"providerbridge/internal/config"
	"providerbridge/internal/format"
	"providerbridge/internal/protocol/anthropic"
	"providerbridge/internal/protocol/chat"
	"providerbridge/internal/protocol/openai"
	"providerbridge/internal/service/stats"
	mbtrace "providerbridge/internal/service/trace"
)

// ============================================================================
// Shared helpers
// ============================================================================

// writeAnthropicError writes an Anthropic-shaped error response.
func writeAnthropicError(w http.ResponseWriter, status int, errType, message string) {
	writeJSON(w, status, map[string]any{
		"type": "error",
		"error": map[string]any{
			"type":    errType,
			"message": message,
		},
	})
}

// inboundAnthropicErrorType maps an error to the Anthropic error type.
func inboundAnthropicErrorType(status int) string {
	switch {
	case status == http.StatusBadRequest:
		return "invalid_request_error"
	case status == http.StatusUnauthorized:
		return "authentication_error"
	case status == http.StatusNotFound:
		return "not_found_error"
	case status == http.StatusRequestEntityTooLarge:
		return "request_too_large"
	case status == http.StatusTooManyRequests:
		return "rate_limit_error"
	case status >= 500:
		return "api_error"
	default:
		return "api_error"
	}
}

// recordInboundCompletion logs completion, records stats, and notifies
// plugins for an inbound request served from a Core result.
func (s *Server) recordInboundCompletion(
	modelAlias string,
	outcome *coreUpstreamOutcome,
	usage format.CoreUsage,
	requestStart time.Time,
	stream bool,
) {
	pm := s.activeProviderManager()
	if s.pluginRegistry != nil {
		freshInput := usage.InputTokens - usage.CachedInputTokens
		if freshInput < 0 {
			freshInput = 0
		}
		billingUsage := stats.BillingUsage{
			FreshInputTokens:     freshInput,
			OutputTokens:         usage.OutputTokens,
			CacheReadInputTokens: usage.CachedInputTokens,
		}
		reqCost := computeCostWithProviderPricing(pm, s.stats, modelAlias, outcome.Preferred.UpstreamModel, outcome.Preferred.ProviderKey, billingUsage)

		status := "success"
		pathLabel := "non-streaming"
		if stream {
			pathLabel = "streaming"
		}
		slog.Info("request completed",
			"request_model", modelAlias,
			"actual_model", outcome.Preferred.UpstreamModel,
			"provider", outcome.Preferred.ProviderKey,
			"inbound", pathLabel,
			"input_total", usage.InputTokens,
			"input_cached_tokens", usage.CachedInputTokens,
			"output_tokens", usage.OutputTokens,
			"request_cost", reqCost,
			"duration", time.Since(requestStart),
		)

		pluginUsage := usageFromAnthropic(outcome.Preferred.Protocol, "core", usage, true)
		s.onRequestCompleted(
			modelAlias, outcome.Preferred.UpstreamModel, outcome.Preferred.ProviderKey,
			requestStart, pluginUsage, reqCost, status, "",
		)

		if s.stats != nil {
			s.stats.Record(modelAlias, outcome.Preferred.UpstreamModel, stats.Usage{
				InputTokens:          usage.InputTokens,
				OutputTokens:         usage.OutputTokens,
				CacheReadInputTokens: usage.CachedInputTokens,
			})
		}
	}
}

// inboundErrorCompletion records a failed inbound request for metrics.
func (s *Server) inboundErrorCompletion(modelAlias string, requestStart time.Time, errMsg string) {
	s.onRequestCompleted(modelAlias, "", "", requestStart, zeroUsage("inbound", "none"), 0, "error", errMsg)
}

// ============================================================================
// Anthropic Messages inbound
// ============================================================================

// handleAnthropicMessages serves POST /v1/messages.
func (s *Server) handleAnthropicMessages(w http.ResponseWriter, r *http.Request) {
	log := slog.Default().With("path", "/v1/messages", "method", r.Method)
	requestStart := time.Now()

	if r.Method != http.MethodPost {
		writeAnthropicError(w, http.StatusMethodNotAllowed, "invalid_request_error", "only POST requests are supported")
		return
	}

	body, err := io.ReadAll(io.LimitReader(r.Body, 100<<20))
	if err != nil {
		log.Error("failed to read request body", "error", err)
		writeAnthropicError(w, http.StatusBadRequest, "invalid_request_error", "failed to read request body")
		return
	}

	anthReq, err := anthropic.ParseMessageRequest(body)
	if err != nil {
		writeAnthropicError(w, http.StatusBadRequest, "invalid_request_error", err.Error())
		return
	}

	modelAlias := anthReq.Model
	sess := s.sessionForRequest(r)

	record := mbtrace.Record{
		HTTPRequest:      mbtrace.NewHTTPRequest(r),
		AnthropicRequest: anthReq,
		Model:            modelAlias,
	}
	defer func() {
		s.writeTrace(record)
	}()

	route, err := s.resolveModelOrFallback(modelAlias)
	if err != nil {
		log.Warn("model resolution failed", "model", modelAlias, "error", err)
		writeAnthropicError(w, http.StatusNotFound, "not_found_error", err.Error())
		s.inboundErrorCompletion(modelAlias, requestStart, "resolve_model")
		return
	}

	clientAdapter, ok := s.adapterRegistry.GetClient(anthropic.ClientProtocolID)
	if !ok {
		writeAnthropicError(w, http.StatusInternalServerError, "api_error", "inbound adapter not available")
		s.inboundErrorCompletion(modelAlias, requestStart, "client_adapter")
		return
	}

	coreReq, err := clientAdapter.ToCoreRequest(r.Context(), anthReq)
	if err != nil {
		writeAnthropicError(w, http.StatusBadRequest, "invalid_request_error", err.Error())
		s.inboundErrorCompletion(modelAlias, requestStart, "to_core_request")
		return
	}

	outcome, err := s.executeCoreUpstream(r.Context(), coreReq, route, modelAlias, sess)
	if err != nil {
		log.Error("upstream execution failed", "error", err)
		record.Error = traceError("core_upstream", err)
		writeAnthropicError(w, http.StatusBadGateway, inboundAnthropicErrorType(http.StatusBadGateway), err.Error())
		s.inboundErrorCompletion(modelAlias, requestStart, "core_upstream")
		return
	}

	if anthReq.Stream {
		streamAdapter, ok := s.adapterRegistry.GetClientStream(anthropic.ClientProtocolID)
		if !ok {
			writeAnthropicError(w, http.StatusInternalServerError, "api_error", "inbound stream adapter not available")
			s.inboundErrorCompletion(modelAlias, requestStart, "client_stream_adapter")
			return
		}
		streamAny, err := streamAdapter.FromCoreStream(r.Context(), coreReq, outcome.CoreEvents)
		if err != nil {
			writeAnthropicError(w, http.StatusInternalServerError, "api_error", err.Error())
			s.inboundErrorCompletion(modelAlias, requestStart, "from_core_stream")
			return
		}
		streamResult, ok := streamAny.(*anthropic.AnthropicStreamResult)
		if !ok {
			writeAnthropicError(w, http.StatusInternalServerError, "api_error", "unexpected stream result type")
			s.inboundErrorCompletion(modelAlias, requestStart, "stream_type")
			return
		}
		usage := s.writeAnthropicSSE(w, r, streamResult, record, outcome)
		s.recordInboundCompletion(modelAlias, outcome, usage, requestStart, true)
		return
	}

	respAny, err := clientAdapter.FromCoreResponse(r.Context(), outcome.CoreResp)
	if err != nil {
		log.Error("failed to convert response", "error", err)
		record.Error = traceError("from_core_response", err)
		writeAnthropicError(w, http.StatusBadGateway, "api_error", err.Error())
		s.inboundErrorCompletion(modelAlias, requestStart, "from_core_response")
		return
	}
	msgResp, ok := respAny.(*anthropic.MessageResponse)
	if !ok {
		writeAnthropicError(w, http.StatusInternalServerError, "api_error", "unexpected response type")
		s.inboundErrorCompletion(modelAlias, requestStart, "response_type")
		return
	}

	record.UpstreamRequest = msgResp
	writeJSON(w, http.StatusOK, msgResp)
	s.recordInboundCompletion(modelAlias, outcome, format.CoreUsage{
		InputTokens:       msgResp.Usage.InputTokens,
		OutputTokens:      msgResp.Usage.OutputTokens,
		TotalTokens:       msgResp.Usage.InputTokens + msgResp.Usage.OutputTokens,
		CachedInputTokens: msgResp.Usage.CacheReadInputTokens,
	}, requestStart, false)
}

// handleAnthropicCountTokens serves POST /v1/messages/count_tokens with a
// character-based estimate. Exact counting is optional for clients; Claude
// Code falls back to its own estimate when the endpoint is absent.
func (s *Server) handleAnthropicCountTokens(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeAnthropicError(w, http.StatusMethodNotAllowed, "invalid_request_error", "only POST requests are supported")
		return
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, 100<<20))
	if err != nil {
		writeAnthropicError(w, http.StatusBadRequest, "invalid_request_error", "failed to read request body")
		return
	}
	var payload struct {
		Messages json.RawMessage `json:"messages"`
		System   json.RawMessage `json:"system"`
		Tools    json.RawMessage `json:"tools"`
	}
	_ = json.Unmarshal(body, &payload)
	estimate := (len(payload.Messages) + len(payload.System) + len(payload.Tools)) / 4
	writeJSON(w, http.StatusOK, map[string]any{"input_tokens": estimate})
}

// writeAnthropicSSE writes the Anthropic SSE stream and returns the final
// usage extracted from the stream events.
func (s *Server) writeAnthropicSSE(
	w http.ResponseWriter,
	r *http.Request,
	result *anthropic.AnthropicStreamResult,
	record mbtrace.Record,
	outcome *coreUpstreamOutcome,
) format.CoreUsage {
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.WriteHeader(http.StatusOK)

	for ev := range result.Chan() {
		if err := writeSSE(w, openai.StreamEvent{Event: ev.Type, Data: ev}); err != nil {
			slog.Default().Warn("SSE write failed; aborting stream", "error", err)
			break
		}
	}

	finalUsage := result.FinalUsage()
	coreUsage := format.CoreUsage{
		InputTokens:       finalUsage.InputTokens,
		OutputTokens:      finalUsage.OutputTokens,
		TotalTokens:       finalUsage.InputTokens + finalUsage.OutputTokens,
		CachedInputTokens: finalUsage.CacheReadInputTokens,
	}

	// DeepSeek reasoning replay: cache reasoning from chat upstream streams.
	if outcome != nil && outcome.ProviderBuf != nil && outcome.Preferred.Protocol == config.ProtocolOpenAIChat {
		s.cacheChatStreamReasoning(outcome, r)
	}

	// Trace capture.
	if s.tracer != nil && s.tracer.Enabled() {
		var anthBuf []anthropic.StreamEvent
		for _, raw := range result.Buffer() {
			if ev, ok := raw.(anthropic.StreamEvent); ok {
				anthBuf = append(anthBuf, ev)
			}
		}
		record.AnthropicStreamEvents = anthBuf
	}

	return coreUsage
}

// cacheChatStreamReasoning extracts reasoning + tool call ids from the chat
// provider buffer and caches them for DeepSeek thinking replay.
func (s *Server) cacheChatStreamReasoning(outcome *coreUpstreamOutcome, r *http.Request) {
	sess := s.sessionForRequest(r)
	if sess == nil || outcome.ProviderBuf == nil {
		return
	}
	raw := outcome.ProviderBuf()
	var streamReasoning string
	var toolCallIDs []string
	seen := make(map[string]struct{})
	for _, item := range raw {
		chunk, ok := item.(chat.ChatStreamChunk)
		if !ok {
			continue
		}
		for _, sc := range chunk.Choices {
			if sc.Delta.ReasoningContent != "" {
				streamReasoning += sc.Delta.ReasoningContent
			}
			for _, tc := range sc.Delta.ToolCalls {
				if tc.ID == "" {
					continue
				}
				if _, dup := seen[tc.ID]; dup {
					continue
				}
				seen[tc.ID] = struct{}{}
				toolCallIDs = append(toolCallIDs, tc.ID)
			}
		}
	}
	if streamReasoning != "" && len(toolCallIDs) > 0 {
		cacheReasoningForChat(sess, toolCallIDs, streamReasoning)
	}
}

// ============================================================================
// OpenAI Chat Completions inbound
// ============================================================================

// handleChatCompletions serves POST /v1/chat/completions.
func (s *Server) handleChatCompletions(w http.ResponseWriter, r *http.Request) {
	log := slog.Default().With("path", "/v1/chat/completions", "method", r.Method)
	requestStart := time.Now()

	if r.Method != http.MethodPost {
		payload := openai.ErrorResponse{Error: openai.ErrorObject{
			Message: "only POST requests are supported", Type: "invalid_request_error", Code: "method_not_allowed",
		}}
		writeOpenAIError(w, http.StatusMethodNotAllowed, payload)
		return
	}

	body, err := io.ReadAll(io.LimitReader(r.Body, 100<<20))
	if err != nil {
		log.Error("failed to read request body", "error", err)
		payload := openai.ErrorResponse{Error: openai.ErrorObject{
			Message: "failed to read request body", Type: "invalid_request_error",
		}}
		writeOpenAIError(w, http.StatusBadRequest, payload)
		return
	}

	chatReq, err := chat.ParseChatCompletionsRequest(body)
	if err != nil {
		payload := openai.ErrorResponse{Error: openai.ErrorObject{
			Message: err.Error(), Type: "invalid_request_error", Code: "invalid_request",
		}}
		writeOpenAIError(w, http.StatusBadRequest, payload)
		return
	}

	modelAlias := chatReq.Model
	sess := s.sessionForRequest(r)

	record := mbtrace.Record{
		HTTPRequest: mbtrace.NewHTTPRequest(r),
		ChatRequest: chatReq,
		Model:       modelAlias,
	}
	defer func() {
		s.writeTrace(record)
	}()

	route, err := s.resolveModelOrFallback(modelAlias)
	if err != nil {
		log.Warn("model resolution failed", "model", modelAlias, "error", err)
		payload := openai.ErrorResponse{Error: openai.ErrorObject{
			Message: err.Error(), Type: "invalid_request_error", Code: "model_not_found",
		}}
		writeOpenAIError(w, http.StatusNotFound, payload)
		s.inboundErrorCompletion(modelAlias, requestStart, "resolve_model")
		return
	}

	clientAdapter, ok := s.adapterRegistry.GetClient(chat.ClientProtocolID)
	if !ok {
		payload := openai.ErrorResponse{Error: openai.ErrorObject{
			Message: "inbound adapter not available", Type: "server_error",
		}}
		writeOpenAIError(w, http.StatusInternalServerError, payload)
		s.inboundErrorCompletion(modelAlias, requestStart, "client_adapter")
		return
	}

	coreReq, err := clientAdapter.ToCoreRequest(r.Context(), chatReq)
	if err != nil {
		payload := openai.ErrorResponse{Error: openai.ErrorObject{
			Message: err.Error(), Type: "invalid_request_error", Code: "conversion_error",
		}}
		writeOpenAIError(w, http.StatusBadRequest, payload)
		s.inboundErrorCompletion(modelAlias, requestStart, "to_core_request")
		return
	}

	outcome, err := s.executeCoreUpstream(r.Context(), coreReq, route, modelAlias, sess)
	if err != nil {
		log.Error("upstream execution failed", "error", err)
		record.Error = traceError("core_upstream", err)
		payload := openai.ErrorResponse{Error: openai.ErrorObject{
			Message: err.Error(), Type: "server_error", Code: "provider_error",
		}}
		writeOpenAIError(w, http.StatusBadGateway, payload)
		s.inboundErrorCompletion(modelAlias, requestStart, "core_upstream")
		return
	}

	if chatReq.Stream {
		streamAdapter, ok := s.adapterRegistry.GetClientStream(chat.ClientProtocolID)
		if !ok {
			payload := openai.ErrorResponse{Error: openai.ErrorObject{
				Message: "inbound stream adapter not available", Type: "server_error",
			}}
			writeOpenAIError(w, http.StatusInternalServerError, payload)
			s.inboundErrorCompletion(modelAlias, requestStart, "client_stream_adapter")
			return
		}
		streamAny, err := streamAdapter.FromCoreStream(r.Context(), coreReq, outcome.CoreEvents)
		if err != nil {
			payload := openai.ErrorResponse{Error: openai.ErrorObject{
				Message: err.Error(), Type: "server_error", Code: "conversion_error",
			}}
			writeOpenAIError(w, http.StatusInternalServerError, payload)
			s.inboundErrorCompletion(modelAlias, requestStart, "from_core_stream")
			return
		}
		streamResult, ok := streamAny.(*chat.ChatClientStreamResult)
		if !ok {
			payload := openai.ErrorResponse{Error: openai.ErrorObject{
				Message: "unexpected stream result type", Type: "server_error",
			}}
			writeOpenAIError(w, http.StatusInternalServerError, payload)
			s.inboundErrorCompletion(modelAlias, requestStart, "stream_type")
			return
		}
		usage := s.writeChatSSE(w, streamResult, record)
		if s.tracer != nil && s.tracer.Enabled() && outcome.ProviderBuf != nil {
			var providerChunks []chat.ChatStreamChunk
			for _, raw := range outcome.ProviderBuf() {
				if chunk, ok := raw.(chat.ChatStreamChunk); ok {
					providerChunks = append(providerChunks, chunk)
				}
			}
			if len(providerChunks) > 0 {
				record.ChatStreamEvents = providerChunks
			}
		}
		s.cacheChatStreamReasoning(outcome, r)
		s.recordInboundCompletion(modelAlias, outcome, usage, requestStart, true)
		return
	}

	respAny, err := clientAdapter.FromCoreResponse(r.Context(), outcome.CoreResp)
	if err != nil {
		log.Error("failed to convert response", "error", err)
		record.Error = traceError("from_core_response", err)
		payload := openai.ErrorResponse{Error: openai.ErrorObject{
			Message: err.Error(), Type: "server_error", Code: "conversion_error",
		}}
		writeOpenAIError(w, http.StatusBadGateway, payload)
		s.inboundErrorCompletion(modelAlias, requestStart, "from_core_response")
		return
	}
	chatResp, ok := respAny.(*chat.ChatResponse)
	if !ok {
		payload := openai.ErrorResponse{Error: openai.ErrorObject{
			Message: "unexpected response type", Type: "server_error",
		}}
		writeOpenAIError(w, http.StatusInternalServerError, payload)
		s.inboundErrorCompletion(modelAlias, requestStart, "response_type")
		return
	}

	record.ChatResponse = chatResp
	writeJSON(w, http.StatusOK, chatResp)

	usage := format.CoreUsage{}
	if chatResp.Usage != nil {
		usage = format.CoreUsage{
			InputTokens:       chatResp.Usage.PromptTokens,
			OutputTokens:      chatResp.Usage.CompletionTokens,
			TotalTokens:       chatResp.Usage.TotalTokens,
			CachedInputTokens: 0,
		}
		if chatResp.Usage.PromptTokensDetails != nil {
			usage.CachedInputTokens = chatResp.Usage.PromptTokensDetails.CachedTokens
		}
	}
	s.recordInboundCompletion(modelAlias, outcome, usage, requestStart, false)
}

// writeChatSSE writes the Chat Completions SSE stream and returns the final
// usage extracted from the stream chunks.
func (s *Server) writeChatSSE(
	w http.ResponseWriter,
	result *chat.ChatClientStreamResult,
	record mbtrace.Record,
) format.CoreUsage {
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.WriteHeader(http.StatusOK)

	var usage format.CoreUsage
	for chunk := range result.Chan() {
		if chunk.Usage != nil {
			usage = format.CoreUsage{
				InputTokens:  chunk.Usage.PromptTokens,
				OutputTokens: chunk.Usage.CompletionTokens,
				TotalTokens:  chunk.Usage.TotalTokens,
			}
			if chunk.Usage.PromptTokensDetails != nil {
				usage.CachedInputTokens = chunk.Usage.PromptTokensDetails.CachedTokens
			}
		}
		payload, err := json.Marshal(chunk)
		if err != nil {
			continue
		}
		if _, err := w.Write([]byte("data: " + string(payload) + "\n\n")); err != nil {
			slog.Default().Warn("SSE write failed; aborting stream", "error", err)
			break
		}
		if flusher, ok := w.(http.Flusher); ok {
			flusher.Flush()
		}
	}
	// Terminal marker required by the Chat Completions streaming protocol.
	_, _ = w.Write([]byte("data: [DONE]\n\n"))
	if flusher, ok := w.(http.Flusher); ok {
		flusher.Flush()
	}

	// Trace capture.
	if s.tracer != nil && s.tracer.Enabled() {
		var chatBuf []chat.ChatStreamChunk
		for _, raw := range result.Buffer() {
			if chunk, ok := raw.(chat.ChatStreamChunk); ok {
				chatBuf = append(chatBuf, chunk)
			}
		}
		record.ChatStreamEvents = chatBuf
	}

	return usage
}
