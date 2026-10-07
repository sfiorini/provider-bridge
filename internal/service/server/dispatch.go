package server

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"providerbridge/internal/config"
	"providerbridge/internal/extension/plugin"
	"providerbridge/internal/logger"
	"providerbridge/internal/protocol/openai"
	"providerbridge/internal/service/provider"
	"providerbridge/internal/service/stats"

	mbtrace "providerbridge/internal/service/trace"
)

func (server *Server) onRequestCompleted(model, actualModel, providerKey string, startTime time.Time, usage plugin.RequestUsage, cost float64, status, errMsg string) {
	if server.pluginRegistry == nil {
		return
	}
	inputTokens := usage.NormalizedInputTokens
	outputTokens := usage.NormalizedOutputTokens
	cacheCreation := usage.NormalizedCacheCreation
	cacheRead := usage.NormalizedCacheRead
	server.pluginRegistry.OnRequestCompleted(
		&plugin.RequestContext{ModelAlias: model},
		plugin.RequestResult{
			Model:         model,
			ActualModel:   actualModel,
			ProviderKey:   providerKey,
			InputTokens:   inputTokens,
			OutputTokens:  outputTokens,
			CacheCreation: cacheCreation,
			CacheRead:     cacheRead,
			Cost:          cost,
			Duration:      time.Since(startTime),
			Status:        status,
			ErrorMessage:  errMsg,
			Usage:         usage,
		},
	)
}
func (server *Server) handleResponses(writer http.ResponseWriter, request *http.Request) {
	log := slog.Default().With("path", request.URL.Path, "method", request.Method, "remote", request.RemoteAddr)
	log.Debug("request received")
	requestStart := time.Now()
	if request.Method != http.MethodPost {
		log.Warn("method not allowed", "method", request.Method)
		writeOpenAIError(writer, http.StatusMethodNotAllowed, openai.ErrorResponse{Error: openai.ErrorObject{
			Message: "method not allowed",
			Type:    "invalid_request_error",
			Code:    "method_not_allowed",
		}})
		return
	}

	server.sessionForRequest(request)

	body, err := io.ReadAll(request.Body)
	record := mbtrace.Record{HTTPRequest: mbtrace.NewHTTPRequest(request), OpenAIRequest: mbtrace.RawJSONOrString(body)}
	if err != nil {
		log.Error("failed to read request body", "error", err)
		payload := openai.ErrorResponse{Error: openai.ErrorObject{
			Message: "failed to read request body",
			Type:    "invalid_request_error",
			Code:    "invalid_request_body",
		}}
		record.Error = traceError("read_openai_request", err)
		record.OpenAIResponse = payload
		server.writeTrace(record)
		writeOpenAIError(writer, http.StatusBadRequest, payload)
		return
	}

	var responsesRequest openai.ResponsesRequest
	if err := json.Unmarshal(body, &responsesRequest); err != nil {
		log.Warn("invalid JSON body", "error", err)
		payload := openai.ErrorResponse{Error: openai.ErrorObject{
			Message: "invalid JSON body",
			Type:    "invalid_request_error",
			Code:    "invalid_json",
		}}
		record.Error = traceError("decode_openai_request", err)
		record.OpenAIResponse = payload
		server.writeTrace(record)
		writeOpenAIError(writer, http.StatusBadRequest, payload)
		return
	}

	record.Model = responsesRequest.Model
	resolvedRoute, resolveErr := server.resolveModelOrFallback(responsesRequest.Model)
	if resolveErr == nil {
		var candidateInfo string
		for i, c := range resolvedRoute.Candidates {
			if i > 0 {
				candidateInfo += ", "
			}
			candidateInfo += c.ProviderKey + "=" + c.UpstreamModel + "(p" + fmt.Sprint(i) + ")"
		}
		log.Debug("route resolution result", "model", responsesRequest.Model, "candidates", candidateInfo)
	}
	if resolveErr != nil {
		log.Warn("requested unknown model", "model", responsesRequest.Model)
		payload := openai.ErrorResponse{Error: openai.ErrorObject{
			Message: fmt.Sprintf("unknown model: %q", responsesRequest.Model),
			Type:    "invalid_request_error",
			Code:    "model_not_found",
		}}
		record.Error = traceError("model_not_found", fmt.Errorf("model %q not found", responsesRequest.Model))
		record.OpenAIResponse = payload
		server.writeTrace(record)
		writeOpenAIError(writer, http.StatusNotFound, payload)
		return
	}

	// Filter candidates by request features (e.g., image input).
	filteredCandidates, filterReason := server.filterCandidatesByInput(resolvedRoute.Candidates, responsesRequest.Input)
	if len(filteredCandidates) == 0 {
		log.Warn("no available provider after filtering", "model", responsesRequest.Model, "reason", filterReason)
		payload := openai.ErrorResponse{Error: openai.ErrorObject{
			Message: fmt.Sprintf("no available provider for model %q with the requested features", responsesRequest.Model),
			Type:    "invalid_request_error",
			Code:    "provider_error",
		}}
		record.Error = traceError("provider_filtered", fmt.Errorf("candidates filtered: %s", filterReason))
		record.OpenAIResponse = payload
		server.writeTrace(record)
		writeOpenAIError(writer, http.StatusBadGateway, payload)
		return
	}
	resolvedRoute.Candidates = filteredCandidates
	if filterReason != "" {
		log.Info("candidate filter", "model", responsesRequest.Model, "reason", filterReason)
	}

	// Protocol branch: get preferred candidate.
	preferred, ok := resolvedRoute.Preferred()
	if ok {
		log.Debug("provider selected", "model", responsesRequest.Model, "provider", preferred.ProviderKey, "upstream", preferred.UpstreamModel)
	}
	if !ok {
		log.Error("model resolution produced no available provider", "model", responsesRequest.Model)
		payload := openai.ErrorResponse{Error: openai.ErrorObject{
			Message: fmt.Sprintf("no available provider for model %q", responsesRequest.Model),
			Type:    "server_error",
			Code:    "provider_error",
		}}
		record.Error = traceError("provider_error", fmt.Errorf("no available provider for %q", responsesRequest.Model))
		record.OpenAIResponse = payload
		server.writeTrace(record)
		writeOpenAIError(writer, http.StatusBadGateway, payload)
		return
	}

	if preferred.Protocol == config.ProtocolOpenAIResponse {
		server.handleOpenAIResponse(writer, request, responsesRequest, resolvedRoute.Candidates, record)
		return
	}

	// Adapter dispatch path for all non-OpenAI-Response protocols.
	if server.adapterRegistry != nil {
		if _, ok := server.adapterRegistry.GetProvider(preferred.Protocol); ok {
			server.handleWithAdapters(writer, request, responsesRequest, resolvedRoute)
			return
		}
	}

	// No adapter path available.
	log.Error("no adapter path configured", "model", responsesRequest.Model, "protocol", preferred.Protocol)
	payload := openai.ErrorResponse{Error: openai.ErrorObject{
		Message: fmt.Sprintf("no adapter path configured for protocol %q", preferred.Protocol),
		Type:    "server_error",
		Code:    "adapter_not_configured",
	}}
	record.Error = traceError("no_adapter_path", fmt.Errorf("no adapter path"))
	record.OpenAIResponse = payload
	server.writeTrace(record)
	writeOpenAIError(writer, http.StatusInternalServerError, payload)
	server.onRequestCompleted(
		responsesRequest.Model, "", "", requestStart,
		zeroUsage("anthropic", "none"), 0, "error", "no adapter path",
	)
}
func (server *Server) writeTrace(record mbtrace.Record) {
	if server.tracer == nil || !server.tracer.Enabled() {
		return
	}
	requestNumber := server.tracer.NextRequestNumber()

	// Chat category: requests/responses for the openai-chat protocol
	if shouldWriteChatTrace(record) {
		server.writeTraceCategory("Chat", requestNumber, mbtrace.Record{
			HTTPRequest:      record.HTTPRequest,
			Model:            record.Model,
			ChatRequest:      record.ChatRequest,
			ChatResponse:     record.ChatResponse,
			ChatStreamEvents: record.ChatStreamEvents,
			Error:            record.Error,
		})
	}

	if shouldWriteResponseTrace(record) {
		server.writeTraceCategory("Response", requestNumber, mbtrace.Record{
			HTTPRequest:        record.HTTPRequest,
			OpenAIRequest:      record.OpenAIRequest,
			Model:              record.Model,
			OpenAIResponse:     record.OpenAIResponse,
			OpenAIStreamEvents: record.OpenAIStreamEvents,
			UpstreamRequest:    record.UpstreamRequest,
			UpstreamResponse:   record.UpstreamResponse,
			Error:              record.Error,
		})
	}
	if shouldWriteAnthropicTrace(record) {
		server.writeTraceCategory("Anthropic", requestNumber, mbtrace.Record{
			HTTPRequest:           record.HTTPRequest,
			AnthropicRequest:      record.AnthropicRequest,
			Model:                 record.Model,
			AnthropicResponse:     record.AnthropicResponse,
			AnthropicStreamEvents: record.AnthropicStreamEvents,
			Error:                 record.Error,
		})
	}
}
func (server *Server) writeTraceCategory(category string, requestNumber uint64, record mbtrace.Record) {
	if _, err := server.tracer.WriteNumbered(category, requestNumber, record); err != nil && server.traceErrors != nil {
		fmt.Fprintf(server.traceErrors, "trace %s write failed: %v\n", category, err)
	}
}
func shouldWriteResponseTrace(record mbtrace.Record) bool {
	return record.OpenAIRequest != nil || record.OpenAIResponse != nil || record.OpenAIStreamEvents != nil || record.UpstreamRequest != nil
}
func shouldWriteAnthropicTrace(record mbtrace.Record) bool {
	return record.AnthropicRequest != nil || record.AnthropicResponse != nil || record.AnthropicStreamEvents != nil
}
func shouldWriteChatTrace(record mbtrace.Record) bool {
	return record.ChatRequest != nil || record.ChatResponse != nil || record.ChatStreamEvents != nil
}

func traceError(stage string, err error) map[string]string {
	return map[string]string{"stage": stage, "message": err.Error()}
}
func writeJSON(writer http.ResponseWriter, status int, payload any) {
	writer.Header().Set("Content-Type", "application/json")
	writer.WriteHeader(status)
	_ = json.NewEncoder(writer).Encode(payload)
}
func writeOpenAIError(writer http.ResponseWriter, status int, payload openai.ErrorResponse) {
	writeJSON(writer, status, payload)
}
func writeSSE(writer http.ResponseWriter, event openai.StreamEvent) error {
	var payload []byte
	if event.Data == nil {
		payload = []byte("{}")
	} else {
		payload, _ = json.Marshal(event.Data)
	}
	if _, err := writer.Write([]byte("event: " + event.Event + "\n")); err != nil {
		return err
	}
	if _, err := writer.Write([]byte("data: " + string(payload) + "\n\n")); err != nil {
		return err
	}
	if flusher, ok := writer.(http.Flusher); ok {
		flusher.Flush()
	}
	return nil
}

func (server *Server) handleOpenAIResponse(writer http.ResponseWriter, request *http.Request, responsesRequest openai.ResponsesRequest, candidates []provider.ProviderCandidate, record mbtrace.Record) {
	proxyStart := time.Now()
	var hookErr string
	var lastErr error
	var lastProviderKey string
	actualModel := "" // updated with the successfully used upstream model
	pm := server.activeProviderManager()
	defer func() {
		if hookErr != "" {
			server.onRequestCompleted(
				responsesRequest.Model, actualModel, lastProviderKey, proxyStart,
				zeroUsage(config.ProtocolOpenAIResponse, "none"), 0, "error", hookErr,
			)
		}
	}()
	log := slog.Default().With("path", request.URL.Path, "method", request.Method)
	if pm == nil {
		log.Error("provider manager for OpenAI Responses passthrough not configured")
		payload := openai.ErrorResponse{Error: openai.ErrorObject{
			Message: "provider routing not configured",
			Type:    "server_error",
			Code:    "internal_error",
		}}
		record.Error = map[string]string{"stage": "openai_provider_config", "message": "provider manager not configured"}
		record.OpenAIResponse = payload
		server.writeTrace(record)
		writeOpenAIError(writer, http.StatusBadGateway, payload)
		hookErr = "provider manager not configured"
		return
	}

	// Filter to only OpenAI-response protocol candidates.
	openaiCandidates := make([]provider.ProviderCandidate, 0, len(candidates))
	for _, c := range candidates {
		if c.Protocol == config.ProtocolOpenAIResponse {
			openaiCandidates = append(openaiCandidates, c)
		}
	}
	if len(openaiCandidates) == 0 {
		log.Error("no provider candidates for the OpenAI Responses protocol")
		payload := openai.ErrorResponse{Error: openai.ErrorObject{
			Message: "no available provider",
			Type:    "server_error",
			Code:    "provider_error",
		}}
		record.Error = map[string]string{"stage": "openai_provider_config", "message": "no openai-response candidates"}
		record.OpenAIResponse = payload
		server.writeTrace(record)
		writeOpenAIError(writer, http.StatusBadGateway, payload)
		hookErr = "no openai-response candidates"
		return
	}

	for i, candidate := range openaiCandidates {
		providerKey := candidate.ProviderKey
		lastProviderKey = providerKey
		isLast := i == len(openaiCandidates)-1
		log := logger.L().With("provider", providerKey, "attempt", i+1)
		if candidate.Client == nil {
			if dynamicClient, err := pm.ClientForKey(providerKey); err == nil {
				candidate.Client = dynamicClient
			}
		}

		baseURL := pm.ProviderBaseURL(providerKey)
		apiKey := pm.ProviderAPIKey(providerKey)
		if baseURL == "" {
			if isLast {
				log.Error("OpenAI provider missing base_url")
				payload := openai.ErrorResponse{Error: openai.ErrorObject{
					Message: "provider not configured",
					Type:    "server_error",
					Code:    "internal_error",
				}}
				record.Error = map[string]string{"stage": "openai_provider_config", "message": "missing base_url"}
				record.OpenAIResponse = payload
				server.writeTrace(record)
				hookErr = "missing base_url"
				writeOpenAIError(writer, http.StatusBadGateway, payload)
				return
			}
			logger.Warn("OpenAI provider missing base_url; trying next candidate",
				"provider", providerKey,
				"request_model", responsesRequest.Model,
				"attempt", i+1)
			lastErr = fmt.Errorf("provider %q has empty base_url", providerKey)
			continue
		}

		// Build upstream URL: baseURL + /v1/responses
		upstreamURL := strings.TrimRight(baseURL, "/")
		if !strings.HasSuffix(upstreamURL, "/v1/responses") && !strings.HasSuffix(upstreamURL, "/responses") {
			upstreamURL += "/v1/responses"
		}

		upstreamRequest := responsesRequest
		upstreamRequest.Model = candidate.UpstreamModel
		actualModel = candidate.UpstreamModel

		// Inject web_search tool if enabled for this model.
		if pm.ResolvedWebSearchForModel(responsesRequest.Model) == "enabled" {
			upstreamRequest.Tools = InjectWebSearchTool(upstreamRequest.Tools)
		}

		body, err := json.Marshal(upstreamRequest)
		if err != nil {
			if isLast {
				log.Error("failed to serialize request", "error", err)
				payload := openai.ErrorResponse{Error: openai.ErrorObject{
					Message: "internal error",
					Type:    "server_error",
					Code:    "internal_error",
				}}
				record.Error = traceError("encode_openai_upstream_request", err)
				record.OpenAIResponse = payload
				hookErr = "encode upstream request"
				server.writeTrace(record)
				writeOpenAIError(writer, http.StatusInternalServerError, payload)
				return
			}
			logger.Warn("OpenAI request serialization failed; trying next candidate",
				"provider", providerKey,
				"request_model", responsesRequest.Model,
				"attempt", i+1,
				"error", err)
			lastErr = err
			continue
		}

		// Create upstream request
		upstreamReq, err := http.NewRequestWithContext(request.Context(), http.MethodPost, upstreamURL, bytes.NewReader(body))
		if err != nil {
			if isLast {
				log.Error("failed to create upstream request", "error", err)
				payload := openai.ErrorResponse{Error: openai.ErrorObject{
					Message: "upstream request failed",
					Type:    "server_error",
					Code:    "internal_error",
				}}
				record.Error = traceError("create_openai_upstream_request", err)
				hookErr = "create upstream request"
				record.OpenAIResponse = payload
				server.writeTrace(record)
				writeOpenAIError(writer, http.StatusBadGateway, payload)
				return
			}
			logger.Warn("OpenAI upstream request creation failed; trying next candidate",
				"provider", providerKey,
				"request_model", responsesRequest.Model,
				"attempt", i+1,
				"error", err)
			lastErr = err
			continue
		}
		upstreamReq.Header.Set("Content-Type", "application/json")
		upstreamReq.Header.Set("Authorization", "Bearer "+apiKey)

		client := server.openAIHTTP
		if client == nil {
			client = &http.Client{Timeout: 0}
		}
		upstreamResp, err := client.Do(upstreamReq)
		if err != nil {
			if isLast {
				log.Error("OpenAI upstream request failed",
					"request_model", responsesRequest.Model,
					"actual_model", upstreamRequest.Model,
					"error", err.Error(),
					"stage", "openai_upstream",
				)
				payload := openai.ErrorResponse{Error: openai.ErrorObject{
					Message: err.Error(),
					Type:    "server_error",
					Code:    "provider_error",
				}}
				hookErr = err.Error()
				record.Error = traceError("openai_upstream", err)
				record.OpenAIResponse = payload
				server.writeTrace(record)
				writeOpenAIError(writer, http.StatusBadGateway, payload)
				return
			}
			logger.Warn("OpenAI upstream connection failed; falling back to next candidate",
				"request_model", responsesRequest.Model,
				"attempt", i+1,
				"provider", providerKey,
				"error", err,
			)
			lastErr = err
			continue
		}
		defer upstreamResp.Body.Close()

		// Log successful fallback if not on the first candidate
		if i > 0 {
			logger.Info("OpenAI fallback succeeded",
				"request_model", responsesRequest.Model,
				"final_provider", providerKey,
				"final_model", candidate.UpstreamModel,
				"attempt", i+1,
			)
		}

		// Copy response headers and status
		for key, values := range upstreamResp.Header {
			for _, v := range values {
				writer.Header().Add(key, v)
			}
		}
		writer.WriteHeader(upstreamResp.StatusCode)

		traceEnabled := server.tracer != nil && server.tracer.Enabled()
		usageEnabled := upstreamResp.StatusCode >= 200 && upstreamResp.StatusCode <= 299 && (server.stats != nil || server.pluginRegistry != nil)
		shouldCapture := traceEnabled || usageEnabled

		var captured bytes.Buffer
		target := io.Writer(writer)
		if shouldCapture {
			target = io.MultiWriter(writer, &captured)
		}
		if _, err := io.Copy(target, upstreamResp.Body); err != nil {
			hookErr = "copy upstream response"
			log.Error("failed to copy upstream response", "error", err)
			return
		}

		if traceEnabled {
			record.OpenAIResponse = mbtrace.RawJSONOrString(captured.Bytes())
			server.writeTrace(record)
		}

		// Capture usage for metrics recording.
		var billingUsage stats.BillingUsage
		var metricTelemetry plugin.RequestUsage
		if usageEnabled {
			if u, raw, source, ok := openAIUsageFromResponse(captured.Bytes(), responsesRequest.Stream); ok {
				billingUsage = u.BillingUsage()
				metricTelemetry = usageFromStats(config.ProtocolOpenAIResponse, source, u, raw)
				if server.stats != nil {
					server.stats.RecordBilling(responsesRequest.Model, actualModel, billingUsage)
					logBillingUsageLine(responsesRequest.Model, actualModel, billingUsage, server.stats)
				}
			}
		}
		if metricTelemetry.Protocol == "" {
			metricTelemetry = zeroUsage(config.ProtocolOpenAIResponse, "none")
		}

		// Record metrics via plugin hooks.
		status := "success"
		errMsg := ""
		if upstreamResp.StatusCode < 200 || upstreamResp.StatusCode >= 300 {
			status = "error"
			errMsg = fmt.Sprintf("HTTP %d", upstreamResp.StatusCode)
		}
		cost := float64(0)
		if server.stats != nil {
			cost = computeCostWithProviderPricing(pm, server.stats, responsesRequest.Model, actualModel, providerKey, billingUsage)
		}
		server.onRequestCompleted(
			responsesRequest.Model, actualModel, providerKey, proxyStart,
			metricTelemetry,
			cost, status, errMsg,
		)

		// Record trace including final provider info
		record.Model = fmt.Sprintf("%s (%s)", responsesRequest.Model, providerKey)

		return // success
	}

	// All candidates failed
	log.Error("all OpenAI Responses provider candidates failed",
		"request_model", responsesRequest.Model,
		"candidates_count", len(openaiCandidates),
		"last_error", lastErr,
	)
	if hookErr == "" {
		hookErr = fmt.Sprintf("all %d candidates failed: %v", len(openaiCandidates), lastErr)
	}
}
