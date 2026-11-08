package anthropic

import "encoding/json"

type CacheControl struct {
	Type string `json:"type"`
	TTL  string `json:"ttl,omitempty"`
}

type MessageRequest struct {
	Model         string          `json:"model"`
	MaxTokens     int             `json:"max_tokens"`
	System        []ContentBlock  `json:"system,omitempty"`
	Messages      []Message       `json:"messages"`
	Tools         []Tool          `json:"tools,omitempty"`
	ToolChoice    *ToolChoice     `json:"tool_choice,omitempty"`
	Temperature   *float64        `json:"temperature,omitempty"`
	TopP          *float64        `json:"top_p,omitempty"`
	TopK          *int            `json:"top_k,omitempty"`
	StopSequences []string        `json:"stop_sequences,omitempty"`
	Metadata      map[string]any  `json:"metadata,omitempty"`
	Stream        bool            `json:"stream,omitempty"`
	CacheControl  *CacheControl   `json:"cache_control,omitempty"`
	Raw           json.RawMessage `json:"-"`
	Thinking      *ThinkingConfig `json:"thinking,omitempty"`
	OutputConfig  *OutputConfig   `json:"output_config,omitempty"`
}

type Message struct {
	Role    string         `json:"role"`
	Content []ContentBlock `json:"content"`
}

type ContentBlock struct {
	Type         string          `json:"type"`
	Text         string          `json:"text,omitempty"`
	Thinking     string          `json:"thinking,omitempty"`
	Signature    string          `json:"signature,omitempty"`
	Source       *ImageSource    `json:"source,omitempty"`
	ID           string          `json:"id,omitempty"`
	Name         string          `json:"name,omitempty"`
	Input        json.RawMessage `json:"input,omitempty"`
	ToolUseID    string          `json:"tool_use_id,omitempty"`
	Content      any             `json:"content,omitempty"`
	CacheControl *CacheControl   `json:"cache_control,omitempty"`
}

type ImageSource struct {
	Type      string `json:"type"`
	URL       string `json:"url,omitempty"`
	MediaType string `json:"media_type,omitempty"`
	Data      string `json:"data,omitempty"`
}

func (block ContentBlock) MarshalJSON() ([]byte, error) {
	type contentBlock ContentBlock
	if block.Type != "thinking" || block.Thinking != "" {
		if block.Type == "text" {
			// Anthropic emits {"type":"text","text":""} for text
			// content_block_start; strict SDKs require the text field even
			// when empty. The shadowing field below re-tags it without
			// omitempty while keeping every other field.
			type textBlock struct {
				contentBlock
				Text string `json:"text"`
			}
			return json.Marshal(textBlock{contentBlock: contentBlock(block), Text: block.Text})
		}
		return json.Marshal(contentBlock(block))
	}
	type thinkingBlock struct {
		Type         string        `json:"type"`
		Thinking     string        `json:"thinking"`
		Signature    string        `json:"signature,omitempty"`
		CacheControl *CacheControl `json:"cache_control,omitempty"`
	}
	return json.Marshal(thinkingBlock{
		Type:         block.Type,
		Thinking:     block.Thinking,
		Signature:    block.Signature,
		CacheControl: block.CacheControl,
	})
}

type Tool struct {
	Name         string         `json:"name"`
	Type         string         `json:"type,omitempty"`
	Description  string         `json:"description,omitempty"`
	InputSchema  map[string]any `json:"input_schema,omitempty"`
	MaxUses      int            `json:"max_uses,omitempty"`
	CacheControl *CacheControl  `json:"cache_control,omitempty"`
}

type ToolChoice struct {
	Type string `json:"type,omitempty"`
	Name string `json:"name,omitempty"`
}

func (choice ToolChoice) IsZero() bool {
	return choice.Type == "" && choice.Name == ""
}

// MessageResponse is the Anthropic Messages response shape. Model, Content,
// StopReason and StopSequence are always emitted (no omitempty): the strict
// SDK Message schema requires id, type, role, content, model, stop_reason,
// stop_sequence and usage to be present (empty string / empty array are
// schema-valid).
type MessageResponse struct {
	ID           string         `json:"id"`
	Type         string         `json:"type"`
	Role         string         `json:"role"`
	Model        string         `json:"model"`
	Content      []ContentBlock `json:"content"`
	StopReason   string         `json:"stop_reason"`
	StopSequence string         `json:"stop_sequence"`
	Usage        Usage          `json:"usage"`
}

type Usage struct {
	InputTokens              int            `json:"input_tokens,omitempty"`
	OutputTokens             int            `json:"output_tokens,omitempty"`
	CacheCreationInputTokens int            `json:"cache_creation_input_tokens,omitempty"`
	CacheReadInputTokens     int            `json:"cache_read_input_tokens,omitempty"`
	CacheCreation            *CacheCreation `json:"cache_creation,omitempty"`
}

type CacheCreation struct {
	Ephemeral5mInputTokens int `json:"ephemeral_5m_input_tokens,omitempty"`
	Ephemeral1hInputTokens int `json:"ephemeral_1h_input_tokens,omitempty"`
}

type StreamEvent struct {
	Type         string           `json:"type"`
	Message      *MessageResponse `json:"message,omitempty"`
	Index        int              `json:"index,omitempty"`
	ContentBlock *ContentBlock    `json:"content_block,omitempty"`
	Delta        StreamDelta      `json:"delta,omitempty"`
	Usage        *Usage           `json:"usage,omitempty"`
	Error        *ErrorObject     `json:"error,omitempty"`
}

type StreamDelta struct {
	Type         string `json:"type,omitempty"`
	Text         string `json:"text,omitempty"`
	Thinking     string `json:"thinking,omitempty"`
	Signature    string `json:"signature,omitempty"`
	PartialJSON  string `json:"partial_json,omitempty"`
	StopReason   string `json:"stop_reason,omitempty"`
	StopSequence string `json:"stop_sequence,omitempty"`
}

// MarshalJSON shapes message_delta deltas to the strict SDK schema: when a
// stop reason is present (message_delta kind), both stop_reason and
// stop_sequence are always emitted. Content deltas keep the existing shape.
func (d StreamDelta) MarshalJSON() ([]byte, error) {
	if d.StopReason != "" {
		return json.Marshal(struct {
			StopReason   string `json:"stop_reason"`
			StopSequence string `json:"stop_sequence"`
		}{StopReason: d.StopReason, StopSequence: d.StopSequence})
	}
	type streamDeltaAlias StreamDelta
	return json.Marshal(streamDeltaAlias(d))
}

type ErrorObject struct {
	Type    string `json:"type,omitempty"`
	Message string `json:"message,omitempty"`
}
type ThinkingConfig struct {
	Type         string `json:"type,omitempty"`
	BudgetTokens int    `json:"budget_tokens,omitempty"`
}

type OutputConfig struct {
	Effort string `json:"effort,omitempty"`
}
