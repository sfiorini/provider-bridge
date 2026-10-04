// Inbound Anthropic Messages client adapter.
//
// Converts Anthropic Messages API requests (POST /v1/messages) to the
// protocol-agnostic Core format, and Core responses/streams back to the
// Anthropic wire format. This mirrors the OpenAI Responses inbound adapter
// (openai.OpenAIAdapter) but for the Anthropic Messages protocol, enabling
// Anthropic-native clients (e.g. Claude Code) to consume any upstream
// provider registered with the bridge.

package anthropic

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sync"

	"moonbridge/internal/format"
)

// ClientProtocolID is the registry key for the inbound Anthropic Messages adapter.
const ClientProtocolID = "anthropic-messages"

// AnthropicClientAdapter converts between the Anthropic Messages wire format
// and Core. It implements format.ClientAdapter and format.ClientStreamAdapter.
type AnthropicClientAdapter struct {
	hooks format.CorePluginHooks
}

// NewAnthropicClientAdapter creates the inbound Anthropic Messages adapter.
func NewAnthropicClientAdapter(hooks format.CorePluginHooks) *AnthropicClientAdapter {
	return &AnthropicClientAdapter{hooks: hooks}
}

// ClientProtocol returns the registry protocol id.
func (a *AnthropicClientAdapter) ClientProtocol() string {
	return ClientProtocolID
}

// ============================================================================
// Request parsing
// ============================================================================

// messageRequestRaw is the wire shape of an inbound Messages request with
// raw JSON where the Anthropic API allows polymorphic values (system and
// message content may be a plain string or an array of content blocks).
type messageRequestRaw struct {
	Model         string          `json:"model"`
	MaxTokens     int             `json:"max_tokens"`
	System        json.RawMessage `json:"system"`
	Messages      []json.RawMessage `json:"messages"`
	Tools         []Tool          `json:"tools"`
	ToolChoice    *ToolChoice     `json:"tool_choice"`
	Temperature   *float64        `json:"temperature"`
	TopP          *float64        `json:"top_p"`
	TopK          *int            `json:"top_k"`
	StopSequences []string        `json:"stop_sequences"`
	Metadata      map[string]any  `json:"metadata"`
	Stream        bool            `json:"stream"`
	Thinking      *ThinkingConfig `json:"thinking"`
	OutputConfig  *OutputConfig   `json:"output_config"`
}

type messageRaw struct {
	Role    string          `json:"role"`
	Content json.RawMessage `json:"content"`
}

// ParseMessageRequest parses an inbound Anthropic Messages request body,
// normalizing polymorphic fields (system as string, message content as
// string) into the canonical block form.
func ParseMessageRequest(body []byte) (*MessageRequest, error) {
	var raw messageRequestRaw
	if err := json.Unmarshal(body, &raw); err != nil {
		return nil, fmt.Errorf("invalid JSON body: %w", err)
	}

	req := &MessageRequest{
		Model:         raw.Model,
		MaxTokens:     raw.MaxTokens,
		Tools:         raw.Tools,
		ToolChoice:    raw.ToolChoice,
		Temperature:   raw.Temperature,
		TopP:          raw.TopP,
		TopK:          raw.TopK,
		StopSequences: raw.StopSequences,
		Metadata:      raw.Metadata,
		Stream:        raw.Stream,
		Thinking:      raw.Thinking,
		OutputConfig:  raw.OutputConfig,
	}

	// system: string | []ContentBlock
	if len(raw.System) > 0 && string(raw.System) != "null" {
		var system string
		if err := json.Unmarshal(raw.System, &system); err == nil {
			req.System = []ContentBlock{{Type: "text", Text: system}}
		} else {
			var blocks []ContentBlock
			if err := json.Unmarshal(raw.System, &blocks); err != nil {
				return nil, fmt.Errorf("invalid system: %w", err)
			}
			req.System = blocks
		}
	}

	// messages: content string | []ContentBlock
	for i, msgRaw := range raw.Messages {
		var m messageRaw
		if err := json.Unmarshal(msgRaw, &m); err != nil {
			return nil, fmt.Errorf("invalid message %d: %w", i, err)
		}
		msg := Message{Role: m.Role}
		if len(m.Content) == 0 || string(m.Content) == "null" {
			return nil, fmt.Errorf("message %d has empty content", i)
		}
		var contentStr string
		if err := json.Unmarshal(m.Content, &contentStr); err == nil {
			msg.Content = []ContentBlock{{Type: "text", Text: contentStr}}
		} else {
			if err := json.Unmarshal(m.Content, &msg.Content); err != nil {
				return nil, fmt.Errorf("invalid message %d content: %w", i, err)
			}
		}
		req.Messages = append(req.Messages, msg)
	}

	if req.Model == "" {
		return nil, fmt.Errorf("model is required")
	}
	if len(req.Messages) == 0 {
		return nil, fmt.Errorf("messages is required")
	}
	if req.MaxTokens <= 0 {
		// Anthropic requires max_tokens; apply a safe default for clients
		// that omit it.
		req.MaxTokens = 8192
	}
	return req, nil
}

// ============================================================================
// ToCoreRequest — anthropic.MessageRequest → CoreRequest
// ============================================================================

// ToCoreRequest converts an inbound Anthropic Messages request to Core.
func (a *AnthropicClientAdapter) ToCoreRequest(ctx context.Context, req any) (*format.CoreRequest, error) {
	anthReq, ok := req.(*MessageRequest)
	if !ok {
		return nil, fmt.Errorf("unexpected request type %T; expected *anthropic.MessageRequest", req)
	}

	coreReq := &format.CoreRequest{
		Model:         anthReq.Model,
		Messages:      make([]format.CoreMessage, 0, len(anthReq.Messages)),
		Temperature:   anthReq.Temperature,
		TopP:          anthReq.TopP,
		TopK:          anthReq.TopK,
		MaxTokens:     anthReq.MaxTokens,
		StopSequences: anthReq.StopSequences,
		Stream:        anthReq.Stream,
		Metadata:      anthReq.Metadata,
	}

	// System blocks.
	for _, block := range anthReq.System {
		if block.Type != "text" {
			continue
		}
		coreReq.System = append(coreReq.System, format.CoreContentBlock{
			Type: "text",
			Text: block.Text,
		})
	}

	// Messages.
	for _, msg := range anthReq.Messages {
		coreMsg := format.CoreMessage{
			Role:    msg.Role,
			Content: convertInboundBlocks(msg.Content),
		}
		coreReq.Messages = append(coreReq.Messages, coreMsg)
	}

	// Tools.
	for _, tool := range anthReq.Tools {
		coreReq.Tools = append(coreReq.Tools, format.CoreTool{
			Name:        tool.Name,
			Description: tool.Description,
			InputSchema: tool.InputSchema,
		})
	}

	// Tool choice.
	if anthReq.ToolChoice != nil && !anthReq.ToolChoice.IsZero() {
		mode := "auto"
		switch anthReq.ToolChoice.Type {
		case "any":
			mode = "any"
		case "tool":
			mode = "required"
		case "none":
			mode = "none"
		}
		raw, _ := json.Marshal(anthReq.ToolChoice)
		coreReq.ToolChoice = &format.CoreToolChoice{
			Mode: mode,
			Name: anthReq.ToolChoice.Name,
			Raw:  raw,
		}
	}

	// Thinking config. "adaptive" (sent by Claude Code for models it does
	// not recognize) maps to nil so the upstream provider default applies.
	if anthReq.Thinking != nil {
		switch anthReq.Thinking.Type {
		case "enabled":
			coreReq.Thinking = &format.CoreThinkingConfig{
				Type:         "enabled",
				BudgetTokens: anthReq.Thinking.BudgetTokens,
			}
		case "disabled":
			coreReq.Thinking = &format.CoreThinkingConfig{Type: "disabled"}
		}
	}

	// Effort.
	if anthReq.OutputConfig != nil && anthReq.OutputConfig.Effort != "" {
		coreReq.Output = &format.CoreOutputConfig{Effort: anthReq.OutputConfig.Effort}
	}

	a.hooks.MutateCoreRequest(ctx, coreReq)

	return coreReq, nil
}

// convertInboundBlocks converts Anthropic content blocks to Core blocks.
// Unknown block types are dropped; redacted thinking cannot be represented
// in Core and is skipped.
func convertInboundBlocks(blocks []ContentBlock) []format.CoreContentBlock {
	out := make([]format.CoreContentBlock, 0, len(blocks))
	for _, block := range blocks {
		switch block.Type {
		case "text":
			out = append(out, format.CoreContentBlock{Type: "text", Text: block.Text})
		case "thinking":
			out = append(out, format.CoreContentBlock{
				Type:               "reasoning",
				ReasoningText:      block.Thinking,
				ReasoningSignature: block.Signature,
			})
		case "image":
			if block.Source == nil {
				continue
			}
			out = append(out, format.CoreContentBlock{
				Type:      "image",
				ImageData: block.Source.Data,
				MediaType: block.Source.MediaType,
			})
		case "tool_use":
			out = append(out, format.CoreContentBlock{
				Type:      "tool_use",
				ToolUseID: block.ID,
				ToolName:  block.Name,
				ToolInput: block.Input,
			})
		case "tool_result":
			out = append(out, format.CoreContentBlock{
				Type:             "tool_result",
				ToolUseID:        block.ToolUseID,
				ToolResultContent: convertToolResultContent(block.Content),
			})
		}
	}
	return out
}

// convertToolResultContent normalizes the polymorphic tool_result content
// field (string | block array) into Core blocks.
func convertToolResultContent(content any) []format.CoreContentBlock {
	switch value := content.(type) {
	case nil:
		return nil
	case string:
		return []format.CoreContentBlock{{Type: "text", Text: value}}
	case []ContentBlock:
		return convertInboundBlocks(value)
	case []any:
		out := make([]format.CoreContentBlock, 0, len(value))
		for _, item := range value {
			raw, err := json.Marshal(item)
			if err != nil {
				continue
			}
			var block ContentBlock
			if err := json.Unmarshal(raw, &block); err != nil {
				continue
			}
			out = append(out, convertInboundBlocks([]ContentBlock{block})...)
		}
		return out
	default:
		return nil
	}
}

// ============================================================================
// FromCoreResponse — CoreResponse → anthropic.MessageResponse
// ============================================================================

// FromCoreResponse converts a Core response to the Anthropic Messages
// non-streaming response shape.
func (a *AnthropicClientAdapter) FromCoreResponse(ctx context.Context, resp *format.CoreResponse) (any, error) {
	if resp == nil {
		return nil, fmt.Errorf("core response is nil")
	}
	if resp.Error != nil {
		return nil, fmt.Errorf("%s", resp.Error.Message)
	}

	message := MessageResponse{
		ID:           newInboundMessageID(resp.ID),
		Type:         "message",
		Role:         "assistant",
		Model:        resp.Model,
		Content:      []ContentBlock{},
		StopSequence: "",
		Usage: Usage{
			InputTokens:  resp.Usage.InputTokens,
			OutputTokens: resp.Usage.OutputTokens,
		},
	}

	hasToolUse := false
	for _, msg := range resp.Messages {
		if msg.Role != "assistant" {
			continue
		}
		for _, block := range msg.Content {
			switch block.Type {
			case "text":
				message.Content = append(message.Content, ContentBlock{Type: "text", Text: block.Text})
			case "reasoning":
				message.Content = append(message.Content, ContentBlock{
					Type:      "thinking",
					Thinking:  block.ReasoningText,
					Signature: block.ReasoningSignature,
				})
			case "tool_use":
				hasToolUse = true
				message.Content = append(message.Content, ContentBlock{
					Type:  "tool_use",
					ID:    block.ToolUseID,
					Name:  block.ToolName,
					Input: block.ToolInput,
				})
			}
		}
	}

	message.StopReason = inboundStopReason(resp.StopReason, resp.Status, hasToolUse)

	return &message, nil
}

// inboundStopReason maps a Core stop reason to the Anthropic vocabulary.
func inboundStopReason(coreReason, status string, hasToolUse bool) string {
	switch coreReason {
	case "end_turn", "tool_use", "max_tokens", "stop_sequence", "content_filter":
		// Already Anthropic-shaped (anthropic upstream passes through).
		if coreReason == "content_filter" {
			return "refusal"
		}
		return coreReason
	case "stop":
		return "end_turn"
	case "tool_calls":
		return "tool_use"
	case "length":
		return "max_tokens"
	case "":
		if status == "incomplete" {
			return "max_tokens"
		}
		if hasToolUse {
			return "tool_use"
		}
		return "end_turn"
	default:
		return coreReason
	}
}

// newInboundMessageID returns an Anthropic-style message id.
func newInboundMessageID(existing string) string {
	if existing != "" {
		return existing
	}
	var buf [12]byte
	if _, err := rand.Read(buf[:]); err != nil {
		return "msg_local"
	}
	return "msg_" + hex.EncodeToString(buf[:])
}

// ============================================================================
// FromCoreStream — CoreStreamEvent → anthropic.StreamEvent (SSE)
// ============================================================================

// AnthropicStreamResult wraps the outbound Anthropic stream channel with
// per-stream buffer access for trace capture and final usage extraction.
type AnthropicStreamResult struct {
	ch  <-chan StreamEvent
	buf func() []any
}

// Chan returns the stream channel.
func (r *AnthropicStreamResult) Chan() <-chan StreamEvent {
	return r.ch
}

// Buffer returns the captured stream events.
func (r *AnthropicStreamResult) Buffer() []any {
	if r.buf == nil {
		return nil
	}
	return r.buf()
}

// FinalUsage extracts usage from the last message_delta / message_start in
// the buffer. Must be called after the stream completes.
func (r *AnthropicStreamResult) FinalUsage() Usage {
	var usage Usage
	for _, raw := range r.Buffer() {
		ev, ok := raw.(StreamEvent)
		if !ok {
			continue
		}
		if ev.Message != nil {
			usage.InputTokens = ev.Message.Usage.InputTokens
		}
		if ev.Usage != nil {
			usage.OutputTokens = ev.Usage.OutputTokens
			if ev.Usage.InputTokens > 0 {
				usage.InputTokens = ev.Usage.InputTokens
			}
		}
	}
	return usage
}

// FromCoreStream converts a Core event stream into an Anthropic Messages
// SSE event stream (message_start → content_block_* → message_delta →
// message_stop, plus ping events).
func (a *AnthropicClientAdapter) FromCoreStream(ctx context.Context, req *format.CoreRequest, events <-chan format.CoreStreamEvent) (any, error) {
	out := make(chan StreamEvent)
	bufReady := make(chan struct{})

	var buf []StreamEvent
	var bufMu sync.Mutex

	go func() {
		defer close(bufReady)
		a.anthropicStreamLoop(ctx, req, events, out, &buf, &bufMu)
	}()

	return &AnthropicStreamResult{
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

// anthropicStreamLoop is the state machine converting Core events to
// Anthropic SSE events.
func (a *AnthropicClientAdapter) anthropicStreamLoop(
	ctx context.Context,
	coreReq *format.CoreRequest,
	events <-chan format.CoreStreamEvent,
	out chan<- StreamEvent,
	buf *[]StreamEvent,
	bufMu *sync.Mutex,
) {
	defer close(out)

	send := func(ev StreamEvent) {
		bufMu.Lock()
		if len(*buf) < 1024 {
			*buf = append(*buf, ev)
		}
		bufMu.Unlock()
		select {
		case <-ctx.Done():
		case out <- ev:
		}
	}

	model := coreReq.Model
	msgID := newInboundMessageID("")

	blockTypes := make(map[int]string) // core block index → "text" | "thinking" | "tool_use"
	started := false
	deltaEmitted := false // message_delta with stop_reason emitted
	var pendingStopReason string
	inputTokens := 0

	for event := range events {
		if a.hooks.OnStreamEvent(ctx, event) {
			continue
		}

		switch event.Type {
		case format.CoreEventCreated:
			if event.Model != "" {
				model = event.Model
			}
			if !started {
				started = true
				send(StreamEvent{
					Type: "message_start",
					Message: &MessageResponse{
						ID:    msgID,
						Type:  "message",
						Role:  "assistant",
						Model: model,
						Usage: Usage{InputTokens: 0, OutputTokens: 0},
					},
				})
			}

		case format.CoreContentBlockStarted:
			if !started {
				started = true
				send(StreamEvent{
					Type: "message_start",
					Message: &MessageResponse{
						ID: msgID, Type: "message", Role: "assistant", Model: model,
						Usage: Usage{InputTokens: 0, OutputTokens: 0},
					},
				})
			}
			if event.ContentBlock == nil {
				continue
			}
			var wireBlock *ContentBlock
			var wireType string
			switch event.ContentBlock.Type {
			case "reasoning":
				wireType = "thinking"
				wireBlock = &ContentBlock{Type: "thinking", Thinking: ""}
			case "tool_use":
				wireType = "tool_use"
				wireBlock = &ContentBlock{
					Type:  "tool_use",
					ID:    event.ContentBlock.ToolUseID,
					Name:  event.ContentBlock.ToolName,
					Input: json.RawMessage("{}"),
				}
			default:
				wireType = "text"
				wireBlock = &ContentBlock{Type: "text", Text: ""}
			}
			blockTypes[event.Index] = wireType
			send(StreamEvent{
				Type:         "content_block_start",
				Index:        event.Index,
				ContentBlock: wireBlock,
			})

		case format.CoreTextDelta:
			blockType := blockTypes[event.Index]
			if blockType == "thinking" || (event.ContentBlock != nil && event.ContentBlock.Type == "reasoning") {
				send(StreamEvent{
					Type:  "content_block_delta",
					Index: event.Index,
					Delta: StreamDelta{Type: "thinking_delta", Thinking: event.Delta},
				})
			} else {
				send(StreamEvent{
					Type:  "content_block_delta",
					Index: event.Index,
					Delta: StreamDelta{Type: "text_delta", Text: event.Delta},
				})
			}

		case format.CoreToolCallArgsDelta:
			send(StreamEvent{
				Type:  "content_block_delta",
				Index: event.Index,
				Delta: StreamDelta{Type: "input_json_delta", PartialJSON: event.Delta},
			})

		case format.CoreContentBlockDone:
			if event.ContentBlock != nil && event.ContentBlock.Type == "reasoning" && event.ContentBlock.ReasoningSignature != "" {
				send(StreamEvent{
					Type:  "content_block_delta",
					Index: event.Index,
					Delta: StreamDelta{Type: "signature_delta", Signature: event.ContentBlock.ReasoningSignature},
				})
			}
			send(StreamEvent{
				Type:  "content_block_stop",
				Index: event.Index,
			})
			delete(blockTypes, event.Index)

		case format.CoreEventInProgress:
			if event.StopReason != "" {
				pendingStopReason = event.StopReason
			}
			if event.Usage != nil {
				if event.Usage.InputTokens > 0 {
					inputTokens = event.Usage.InputTokens
				}
			}
			// Mirror upstream message_delta timing: emit stop reason and
			// output usage as soon as they are known.
			if pendingStopReason != "" && !deltaEmitted {
				deltaEmitted = true
				usage := Usage{InputTokens: inputTokens}
				if event.Usage != nil {
					usage.OutputTokens = event.Usage.OutputTokens
				}
				send(StreamEvent{
					Type:  "message_delta",
					Delta: StreamDelta{StopReason: inboundStopReason(pendingStopReason, "", false)},
					Usage: &usage,
				})
			}

		case format.CoreEventCompleted:
			if event.Model != "" {
				model = event.Model
			}
			if !deltaEmitted {
				if event.StopReason != "" {
					pendingStopReason = event.StopReason
				}
				stop := pendingStopReason
				if stop == "" {
					stop = "end_turn"
				}
				usage := Usage{}
				if event.Usage != nil {
					usage.OutputTokens = event.Usage.OutputTokens
					if event.Usage.InputTokens > 0 {
						inputTokens = event.Usage.InputTokens
					}
				}
				send(StreamEvent{
					Type:  "message_delta",
					Delta: StreamDelta{StopReason: inboundStopReason(stop, "", false)},
					Usage: &usage,
				})
			}
			send(StreamEvent{Type: "message_stop"})

		case format.CoreEventIncomplete:
			if !deltaEmitted {
				send(StreamEvent{
					Type:  "message_delta",
					Delta: StreamDelta{StopReason: "max_tokens"},
					Usage: &Usage{},
				})
			}
			send(StreamEvent{Type: "message_stop"})

		case format.CoreEventFailed:
			errType := "api_error"
			msg := "upstream error"
			if event.Error != nil {
				if event.Error.Message != "" {
					msg = event.Error.Message
				}
				if event.Error.Type != "" {
					errType = event.Error.Type
				}
			}
			send(StreamEvent{
				Type:  "error",
				Error: &ErrorObject{Type: errType, Message: msg},
			})

		case format.CorePing:
			send(StreamEvent{Type: "ping"})
		}
	}
}
