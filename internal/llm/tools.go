package llm

import "encoding/json"

// ToolDefinition defines a tool available to the LLM (Anthropic format).
type ToolDefinition struct {
	Name        string       `json:"name"`
	Description string       `json:"description"`
	InputSchema InputSchema  `json:"input_schema"`
}

type InputSchema struct {
	Type       string                 `json:"type"`
	Properties map[string]SchemaProp  `json:"properties"`
	Required   []string               `json:"required,omitempty"`
}

type SchemaProp struct {
	Type        string   `json:"type"`
	Description string   `json:"description"`
	Enum        []string `json:"enum,omitempty"`
}

// ToolUseBlock is a tool call from the LLM response.
type ToolUseBlock struct {
	Type  string         `json:"type"`
	ID    string         `json:"id"`
	Name  string         `json:"name"`
	Input json.RawMessage `json:"input"`
}

// ToolResultBlock is sent back to the LLM after executing a tool.
type ToolResultBlock struct {
	Type      string `json:"type"`
	ToolUseID string `json:"tool_use_id"`
	Content   string `json:"content"`
}

// ChatResponse holds the parsed LLM response.
type ChatResponse struct {
	Text       string
	ToolUses   []ToolUseBlock
	StopReason string
	AllContent []interface{} // raw content blocks (thinking+text+tool_use) for tool callbacks
}
