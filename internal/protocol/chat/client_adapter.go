// Inbound OpenAI Chat Completions client adapter.
//
// Converts Chat Completions requests (POST /v1/chat/completions) to the
// protocol-agnostic Core format, and Core responses/streams back to the
// Chat Completions wire format. This enables any OpenAI-compatible client
// (e.g. LibreChat) to consume every upstream provider registered with the
// bridge.

package chat

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"time"

	"providerbridge/internal/format"
)

// ClientProtocolID is the registry key for the inbound Chat Completions adapter.
const ClientProtocolID = "chat-completions"

// ChatClientAdapter converts between the Chat Completions wire format and
// Core. It implements format.ClientAdapter and format.ClientStreamAdapter.
type ChatClientAdapter struct {
	hooks format.CorePluginHooks
}

// NewChatClientAdapter creates the inbound Chat Completions adapter.
func NewChatClientAdapter(hooks format.CorePluginHooks) *ChatClientAdapter {
	return &ChatClientAdapter{hooks: hooks}
}

// ClientProtocol returns the registry protocol id.
func (a *ChatClientAdapter) ClientProtocol() string {
	return ClientProtocolID
}

// jsonQuote wraps a raw JSON value into its JSON string representation
// (e.g. {"a":1} becomes "{\"a\":1}"), the form OpenAI Chat Completions uses
// for tool call arguments.
func jsonQuote(raw []byte) json.RawMessage {
	if len(raw) == 0 || string(raw) == "null" {
		return json.RawMessage(`""`)
	}
	quoted, err := json.Marshal(string(raw))
	if err != nil {
		return json.RawMessage(`""`)
	}
	return json.RawMessage(quoted)
}


// ============================================================================
// Request parsing
// ============================================================================

// ParseChatCompletionsRequest parses an inbound Chat Completions body into
// a ChatRequest. It normalizes the legacy "max_tokens" field (sent by most
// clients) into "max_completion_tokens", which is the field the upstream
// wire type binds.
func ParseChatCompletionsRequest(body []byte) (*ChatRequest, error) {
	var probe struct {
		MaxTokens         json.RawMessage `json:"max_tokens"`
		MaxCompletion     json.RawMessage `json:"max_completion_tokens"`
	}
	if err := json.Unmarshal(body, &probe); err != nil {
		return nil, fmt.Errorf("invalid JSON body: %w", err)
	}
	normalized := body
	if len(probe.MaxTokens) > 0 && string(probe.MaxTokens) != "null" &&
		(len(probe.MaxCompletion) == 0 || string(probe.MaxCompletion) == "null") {
		var raw map[string]json.RawMessage
		if err := json.Unmarshal(body, &raw); err != nil {
			return nil, fmt.Errorf("invalid JSON body: %w", err)
		}
		raw["max_completion_tokens"] = probe.MaxTokens
		delete(raw, "max_tokens")
		fixed, err := json.Marshal(raw)
		if err != nil {
			return nil, fmt.Errorf("invalid JSON body: %w", err)
		}
		normalized = fixed
	}

	var req ChatRequest
	if err := json.Unmarshal(normalized, &req); err != nil {
		return nil, fmt.Errorf("invalid JSON body: %w", err)
	}
	if req.Model == "" {
		return nil, fmt.Errorf("model is required")
	}
	if len(req.Messages) == 0 {
		return nil, fmt.Errorf("messages is required")
	}
	return &req, nil
}

// ============================================================================
// ToCoreRequest — chat.ChatRequest → CoreRequest
// ============================================================================

// ToCoreRequest converts an inbound Chat Completions request to Core.
func (a *ChatClientAdapter) ToCoreRequest(ctx context.Context, req any) (*format.CoreRequest, error) {
	chatReq, ok := req.(*ChatRequest)
	if !ok {
		return nil, fmt.Errorf("unexpected request type %T; expected *chat.ChatRequest", req)
	}

	coreReq := &format.CoreRequest{
		Model:         chatReq.Model,
		Messages:      make([]format.CoreMessage, 0, len(chatReq.Messages)),
		Temperature:   chatReq.Temperature,
		TopP:          chatReq.TopP,
		MaxTokens:     chatReq.MaxTokens,
		StopSequences: chatReq.Stop,
		Stream:        chatReq.Stream,
		Metadata:      chatReq.Metadata,
	}

	if chatReq.StreamOptions != nil && chatReq.StreamOptions.IncludeUsage {
		coreReq.Extensions = map[string]any{
			"chat": map[string]any{"include_usage": true},
		}
	}

	for _, msg := range chatReq.Messages {
		switch msg.Role {
		case "system", "developer":
			coreReq.System = append(coreReq.System, format.CoreContentBlock{
				Type: "text",
				Text: inboundContentToString(msg.Content),
			})
			continue
		}

		coreMsg := format.CoreMessage{Role: msg.Role}

		// Reasoning first (assistant reasoning_content precedes content).
		if msg.ReasoningContent != "" {
			coreMsg.Content = append(coreMsg.Content, format.CoreContentBlock{
				Type:          "reasoning",
				ReasoningText: msg.ReasoningContent,
			})
		}

		// Body content (text and optional images).
		coreMsg.Content = append(coreMsg.Content, inboundContentToBlocks(msg.Content)...)

		// Assistant tool calls.
		for _, tc := range msg.ToolCalls {
			coreMsg.Content = append(coreMsg.Content, format.CoreContentBlock{
				Type:      "tool_use",
				ToolUseID: tc.ID,
				ToolName:  tc.Function.Name,
				ToolInput: tc.Function.Arguments,
			})
		}

		// Tool result message.
		if msg.Role == "tool" {
			coreMsg.Content = []format.CoreContentBlock{{
				Type: "tool_result",
				ToolUseID: msg.ToolCallID,
				ToolResultContent: []format.CoreContentBlock{{
					Type: "text",
					Text: inboundContentToString(msg.Content),
				}},
			}}
		}

		coreReq.Messages = append(coreReq.Messages, coreMsg)
	}

	// Tools. Function tools map directly; the OpenAI web_search tool types
	// become a placeholder that the server-side injection replaces with
	// tavily_search / firecrawl_fetch.
	for _, tool := range chatReq.Tools {
		if tool.Type == "web_search" || tool.Type == "web_search_preview" {
			coreReq.Tools = append(coreReq.Tools, format.CoreTool{Name: tool.Type})
			continue
		}
		if tool.Type != "" && tool.Type != "function" {
			continue
		}
		if tool.Function.Name == "" {
			continue
		}
		coreReq.Tools = append(coreReq.Tools, format.CoreTool{
			Name:        tool.Function.Name,
			Description: tool.Function.Description,
			InputSchema: tool.Function.Parameters,
		})
	}

	// Tool choice (raw passthrough with parsed mode).
	if len(chatReq.ToolChoice) > 0 && string(chatReq.ToolChoice) != "null" && string(chatReq.ToolChoice) != "\"none\"" {
		coreReq.ToolChoice = parseInboundToolChoice(chatReq.ToolChoice)
	}

	// Reasoning effort: surfaced for both the anthropic upstream (Core
	// Output.Effort) and the openai-chat upstream (Extensions, mirroring the
	// Responses inbound convention).
	if chatReq.ReasoningEffort != "" {
		coreReq.Output = &format.CoreOutputConfig{Effort: chatReq.ReasoningEffort}
		if coreReq.Extensions == nil {
			coreReq.Extensions = make(map[string]any)
		}
		openaiExt, _ := coreReq.Extensions["openai"].(map[string]any)
		if openaiExt == nil {
			openaiExt = make(map[string]any)
			coreReq.Extensions["openai"] = openaiExt
		}
		reasoning, _ := openaiExt["reasoning"].(map[string]any)
		if reasoning == nil {
			reasoning = make(map[string]any)
			openaiExt["reasoning"] = reasoning
		}
		reasoning["effort"] = chatReq.ReasoningEffort
	}

	a.hooks.MutateCoreRequest(ctx, coreReq)

	return coreReq, nil
}

// inboundContentToString flattens polymorphic message content to a string.
func inboundContentToString(content any) string {
	switch value := content.(type) {
	case nil:
		return ""
	case string:
		return value
	case []ContentPart:
		parts := make([]string, 0, len(value))
		for _, part := range value {
			if part.Type == "text" {
				parts = append(parts, part.Text)
			}
		}
		return strings.Join(parts, "\n")
	case []any:
		parts := make([]string, 0, len(value))
		for _, item := range value {
			raw, err := json.Marshal(item)
			if err != nil {
				continue
			}
			var part ContentPart
			if err := json.Unmarshal(raw, &part); err != nil {
				continue
			}
			if part.Type == "text" {
				parts = append(parts, part.Text)
			}
		}
		return strings.Join(parts, "\n")
	default:
		return fmt.Sprintf("%v", value)
	}
}

// inboundContentToBlocks converts polymorphic message content to Core blocks
// (text and image_url parts).
func inboundContentToBlocks(content any) []format.CoreContentBlock {
	switch value := content.(type) {
	case nil:
		return nil
	case string:
		if value == "" {
			return nil
		}
		return []format.CoreContentBlock{{Type: "text", Text: value}}
	case []ContentPart:
		out := make([]format.CoreContentBlock, 0, len(value))
		for _, part := range value {
			out = append(out, inboundContentPartToBlock(part)...)
		}
		return out
	case []any:
		out := make([]format.CoreContentBlock, 0, len(value))
		for _, item := range value {
			raw, err := json.Marshal(item)
			if err != nil {
				continue
			}
			var part ContentPart
			if err := json.Unmarshal(raw, &part); err != nil {
				continue
			}
			out = append(out, inboundContentPartToBlock(part)...)
		}
		return out
	default:
		return nil
	}
}

func inboundContentPartToBlock(part ContentPart) []format.CoreContentBlock {
	switch part.Type {
	case "text":
		return []format.CoreContentBlock{{Type: "text", Text: part.Text}}
	case "image_url":
		if part.ImageURL == nil || part.ImageURL.URL == "" {
			return nil
		}
		mediaType := "image/png"
		if strings.HasPrefix(part.ImageURL.URL, "data:image/") {
			mediaType = strings.TrimPrefix(strings.SplitN(part.ImageURL.URL, ";", 2)[0], "data:")
		}
		return []format.CoreContentBlock{
			{Type: "image", ImageData: part.ImageURL.URL, MediaType: mediaType},
		}
	default:
		return nil
	}
}

// parseInboundToolChoice parses a raw chat tool_choice value into Core.
func parseInboundToolChoice(raw json.RawMessage) *format.CoreToolChoice {
	var scalar string
	if err := json.Unmarshal(raw, &scalar); err == nil {
		mode := scalar
		if mode == "" || mode == "required" {
			// "required" maps to Core "required"; empty falls back to auto.
			if mode == "" {
				mode = "auto"
			}
		}
		return &format.CoreToolChoice{Mode: mode, Raw: raw}
	}
	var structured struct {
		Type     string `json:"type"`
		Function struct {
			Name string `json:"name"`
		} `json:"function"`
	}
	if err := json.Unmarshal(raw, &structured); err == nil {
		mode := structured.Type
		switch mode {
		case "function", "tool":
			mode = "required"
		case "":
			mode = "auto"
		}
		return &format.CoreToolChoice{Mode: mode, Name: structured.Function.Name, Raw: raw}
	}
	return &format.CoreToolChoice{Mode: "auto", Raw: raw}
}

// ============================================================================
// FromCoreResponse — CoreResponse → chat.ChatResponse
// ============================================================================

// FromCoreResponse converts a Core response to the Chat Completions
// non-streaming response shape.
func (a *ChatClientAdapter) FromCoreResponse(ctx context.Context, resp *format.CoreResponse) (any, error) {
	if resp == nil {
		return nil, fmt.Errorf("core response is nil")
	}
	if resp.Error != nil {
		return nil, fmt.Errorf("%s", resp.Error.Message)
	}

	message := ChatMessage{Role: "assistant"}
	var toolCalls []ToolCall
	textParts := make([]string, 0, 2)
	reasoningParts := make([]string, 0, 2)

	for _, msg := range resp.Messages {
		if msg.Role != "assistant" {
			continue
		}
		for _, block := range msg.Content {
			switch block.Type {
			case "text":
				textParts = append(textParts, block.Text)
			case "reasoning":
				reasoningParts = append(reasoningParts, block.ReasoningText)
			case "tool_use":
				toolCalls = append(toolCalls, ToolCall{
					ID:   block.ToolUseID,
					Type: "function",
					Function: ToolCallFunc{
						Name:      block.ToolName,
						Arguments: jsonQuote(block.ToolInput),
					},
				})
			}
		}
	}

	message.Content = strings.Join(textParts, "")
	if len(reasoningParts) > 0 {
		message.ReasoningContent = strings.Join(reasoningParts, "")
	}
	if len(toolCalls) > 0 {
		message.ToolCalls = toolCalls
	}
	if message.Content == "" && len(toolCalls) == 0 && message.ReasoningContent == "" {
		message.Content = ""
	}

	finish := inboundFinishReason(resp.StopReason, resp.Status)

	chatResp := &ChatResponse{
		ID:      newInboundCompletionID(resp.ID),
		Object:  "chat.completion",
		Created: time.Now().Unix(),
		Model:   resp.Model,
		Choices: []Choice{{
			Index:        0,
			Message:      message,
			FinishReason: finish,
		}},
	}
	if resp.Usage.InputTokens > 0 || resp.Usage.OutputTokens > 0 {
		chatResp.Usage = &Usage{
			PromptTokens:     resp.Usage.InputTokens,
			CompletionTokens: resp.Usage.OutputTokens,
			TotalTokens:      resp.Usage.TotalTokens,
			PromptTokensDetails: &PromptTokensDetails{
				CachedTokens: resp.Usage.CachedInputTokens,
			},
		}
	}
	return chatResp, nil
}

// inboundFinishReason maps a Core stop reason to the Chat Completions
// finish_reason vocabulary.
func inboundFinishReason(coreReason, status string) string {
	switch coreReason {
	case "end_turn", "":
		if status == "incomplete" {
			return "length"
		}
		return "stop"
	case "stop":
		return "stop"
	case "tool_use", "tool_calls":
		return "tool_calls"
	case "max_tokens", "length":
		return "length"
	case "content_filter", "refusal":
		return "content_filter"
	case "stop_sequence":
		return "stop"
	default:
		return "stop"
	}
}

func newInboundCompletionID(existing string) string {
	if existing != "" {
		return existing
	}
	var buf [12]byte
	if _, err := rand.Read(buf[:]); err != nil {
		return "chatcmpl-local"
	}
	return "chatcmpl-" + hex.EncodeToString(buf[:])
}

// ============================================================================
// FromCoreStream — CoreStreamEvent → chat.ChatStreamChunk (SSE)
// ============================================================================

// ChatClientStreamResult wraps the outbound stream channel with buffer access.
type ChatClientStreamResult struct {
	ch  <-chan ChatStreamChunk
	buf func() []any
}

// Chan returns the stream channel.
func (r *ChatClientStreamResult) Chan() <-chan ChatStreamChunk {
	return r.ch
}

// Buffer returns the captured stream chunks.
func (r *ChatClientStreamResult) Buffer() []any {
	if r.buf == nil {
		return nil
	}
	return r.buf()
}

// FromCoreStream converts a Core event stream into Chat Completions chunks.
func (a *ChatClientAdapter) FromCoreStream(ctx context.Context, req *format.CoreRequest, events <-chan format.CoreStreamEvent) (any, error) {
	out := make(chan ChatStreamChunk)
	bufReady := make(chan struct{})

	var buf []ChatStreamChunk
	var bufMu sync.Mutex

	go func() {
		defer close(bufReady)
		a.chatStreamLoop(ctx, req, events, out, &buf, &bufMu)
	}()

	return &ChatClientStreamResult{
		ch: out,
		buf: func() []any {
			<-bufReady
			bufMu.Lock()
			defer bufMu.Unlock()
			result := make([]any, len(buf))
			for i, ev := range buf {
				result[i] = ev
			}
			return result
		},
	}, nil
}

func (a *ChatClientAdapter) chatStreamLoop(
	ctx context.Context,
	coreReq *format.CoreRequest,
	events <-chan format.CoreStreamEvent,
	out chan<- ChatStreamChunk,
	buf *[]ChatStreamChunk,
	bufMu *sync.Mutex,
) {
	defer close(out)

	send := func(chunk ChatStreamChunk) {
			bufMu.Lock()
		if len(*buf) < 1024 {
			*buf = append(*buf, chunk)
		}
		bufMu.Unlock()
		select {
		case <-ctx.Done():
		case out <- chunk:
		}
	}

	id := newInboundCompletionID("")
	created := time.Now().Unix()
	model := coreReq.Model
	includeUsage := false
	if chatExt, ok := coreReq.Extensions["chat"].(map[string]any); ok {
		includeUsage, _ = chatExt["include_usage"].(bool)
	}

	roleSent := false
	nextChunk := func(delta Delta) ChatStreamChunk {
		if !roleSent {
			roleSent = true
			delta.Role = "assistant"
		}
		return ChatStreamChunk{
			ID:      id,
			Object:  "chat.completion.chunk",
			Created: created,
			Model:   model,
			Choices: []StreamChoice{{Index: 0, Delta: delta}},
		}
	}

	blockTypes := make(map[int]string) // core block index → block type
	toolIndexes := make(map[int]int)   // core block index → chat tool_calls index
	nextToolIndex := 0
	var stopReason string
	var usage *Usage

	for event := range events {
			if a.hooks.OnStreamEvent(ctx, event) {
			continue
		}

		switch event.Type {
		case format.CoreEventCreated:
			if event.Model != "" {
				model = event.Model
			}

		case format.CoreContentBlockStarted:
			if event.ContentBlock == nil {
				continue
			}
			blockTypes[event.Index] = event.ContentBlock.Type
			if event.ContentBlock.Type == "tool_use" {
					index := nextToolIndex
				nextToolIndex++
				toolIndexes[event.Index] = index
				send(nextChunk(Delta{ToolCalls: []ToolCall{{
					Index:    &index,
					ID:       event.ContentBlock.ToolUseID,
					Type:     "function",
					Function: ToolCallFunc{Name: event.ContentBlock.ToolName, Arguments: json.RawMessage(`""`)},
				}}}))
			}

		case format.CoreTextDelta:
			blockType := blockTypes[event.Index]
			if blockType == "reasoning" || (event.ContentBlock != nil && event.ContentBlock.Type == "reasoning") {
				send(nextChunk(Delta{ReasoningContent: event.Delta}))
			} else {
				send(nextChunk(Delta{Content: event.Delta}))
			}

		case format.CoreToolCallArgsDelta:
			index, ok := toolIndexes[event.Index]
			if !ok {
				index = nextToolIndex
				nextToolIndex++
				toolIndexes[event.Index] = index
			}
			send(nextChunk(Delta{ToolCalls: []ToolCall{{
				Index:    &index,
				Function: ToolCallFunc{Arguments: jsonQuote([]byte(event.Delta))},
			}}}))

		case format.CoreToolCallArgsDone:
			// The visual-path synthesizer emits complete arguments in one
			// event instead of incremental deltas.
			index, ok := toolIndexes[event.Index]
			if !ok {
				index = nextToolIndex
				nextToolIndex++
				toolIndexes[event.Index] = index
			}
			send(nextChunk(Delta{ToolCalls: []ToolCall{{
				Index:    &index,
				Function: ToolCallFunc{Arguments: jsonQuote([]byte(event.Delta))},
			}}}))

		case format.CoreContentBlockDone:
			// Chat Completions has no per-block close event.
			if event.StopReason != "" {
				stopReason = event.StopReason
			}

		case format.CoreEventInProgress:
			if event.StopReason != "" {
				stopReason = event.StopReason
			}
			if event.Usage != nil {
				usage = coreUsageToChat(event.Usage)
			}

		case format.CoreEventCompleted:
			if event.Model != "" {
				model = event.Model
			}
			if event.StopReason != "" {
				stopReason = event.StopReason
			}
			if event.Usage != nil {
				usage = coreUsageToChat(event.Usage)
			}
			if stopReason == "" {
				stopReason = "end_turn"
			}
			final := ChatStreamChunk{
				ID:      id,
				Object:  "chat.completion.chunk",
				Created: created,
				Model:   model,
				Choices: []StreamChoice{{
					Index:        0,
					Delta:         Delta{},
					FinishReason: inboundFinishReason(stopReason, "completed"),
				}},
			}
			if includeUsage && usage != nil {
				final.Usage = usage
			}
			send(final)

		case format.CoreEventIncomplete:
			send(ChatStreamChunk{
				ID: id, Object: "chat.completion.chunk", Created: created, Model: model,
				Choices: []StreamChoice{{Index: 0, Delta: Delta{}, FinishReason: "length"}},
			})

		case format.CoreEventFailed:
			errType := "server_error"
			msg := "upstream error"
			if event.Error != nil {
				if event.Error.Message != "" {
					msg = event.Error.Message
				}
				if event.Error.Type != "" {
					errType = event.Error.Type
				}
			}
			// Mid-stream errors are delivered as an error payload on the
			// data channel, which OpenAI-compatible clients understand.
			send(ChatStreamChunk{
				ID: id, Object: "chat.completion.chunk", Created: created, Model: model,
				Choices: []StreamChoice{{Index: 0, Delta: Delta{}, FinishReason: "error"}},
			})
			_ = errType
			_ = msg

		case format.CorePing:
			// Chat Completions has no ping event; ignore.
		}
	}
}

func coreUsageToChat(usage *format.CoreUsage) *Usage {
	u := &Usage{
		PromptTokens:     usage.InputTokens,
		CompletionTokens: usage.OutputTokens,
		TotalTokens:      usage.TotalTokens,
	}
	if u.TotalTokens == 0 {
		u.TotalTokens = u.PromptTokens + u.CompletionTokens
	}
	if usage.CachedInputTokens > 0 {
		u.PromptTokensDetails = &PromptTokensDetails{CachedTokens: usage.CachedInputTokens}
	}
	return u
}
