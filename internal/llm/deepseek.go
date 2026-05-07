package llm

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"time"
)

// Message represents a chat message in Anthropic-compatible format.
type Message struct {
	Role    string      `json:"role"`
	Content interface{} `json:"content"` // string or []interface{} for tool results
}

// ChatRequest is the request body for Anthropic Messages API.
type ChatRequest struct {
	Model       string            `json:"model"`
	Messages    []Message         `json:"messages"`
	System      string            `json:"system,omitempty"`
	MaxTokens   int               `json:"max_tokens,omitempty"`
	Temperature float64           `json:"temperature,omitempty"`
	Tools       []ToolDefinition  `json:"tools,omitempty"`
}

type anthropicChatResponse struct {
	Content    []json.RawMessage `json:"content"`
	StopReason string            `json:"stop_reason"`
	Error      *struct {
		Message string `json:"type"`
		Type    string `json:"message"`
	} `json:"error,omitempty"`
}

type anthropicTextBlock struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

type anthropicThinkingBlock struct {
	Type     string `json:"type"`
	Thinking string `json:"thinking"`
}

type anthropicToolUseBlock struct {
	Type  string          `json:"type"`
	ID    string          `json:"id"`
	Name  string          `json:"name"`
	Input json.RawMessage `json:"input"`
}

// EmbeddingRequest is the request body for embeddings (OpenAI format).
type EmbeddingRequest struct {
	Model string `json:"model"`
	Input string `json:"input"`
}

type embeddingResponse struct {
	Data []struct {
		Embedding []float64 `json:"embedding"`
	} `json:"data"`
	Error *struct {
		Message string `json:"message"`
		Type    string `json:"type"`
	} `json:"error,omitempty"`
}

// Client wraps both DeepSeek Anthropic endpoint (chat) and OpenAI endpoint (embeddings).
type Client struct {
	APIKey      string
	ChatURL     string // Anthropic-compatible endpoint for chat
	EmbedURL    string // OpenAI-compatible endpoint for embeddings
	EmbedAPIKey string // separate API key for embeddings (falls back to APIKey if empty)
	HTTP        *http.Client
}

// NewClient creates a new DeepSeek API client.
func NewClient(apiKey, chatURL, embedURL, embedAPIKey string) *Client {
	return &Client{
		APIKey:      apiKey,
		ChatURL:     chatURL,
		EmbedURL:    embedURL,
		EmbedAPIKey: embedAPIKey,
		HTTP:        &http.Client{Timeout: 60 * time.Second},
	}
}

// ChatCompletion sends a chat completion request (Anthropic Messages API format).
func (c *Client) ChatCompletion(req *ChatRequest) (*ChatResponse, error) {
	url := c.ChatURL + "/v1/messages"

	body := map[string]interface{}{
		"model":      req.Model,
		"messages":   req.Messages,
		"max_tokens": req.MaxTokens,
	}
	if req.System != "" {
		body["system"] = req.System
	}
	if req.Temperature > 0 {
		body["temperature"] = req.Temperature
	}
	if len(req.Tools) > 0 {
		body["tools"] = req.Tools
	}

	data, err := json.Marshal(body)
	if err != nil {
		return nil, fmt.Errorf("marshal request: %w", err)
	}

	slog.Info("chat request", "url", url, "tools", len(req.Tools), "body", string(data))

	httpReq, err := http.NewRequest("POST", url, bytes.NewReader(data))
	if err != nil {
		return nil, fmt.Errorf("create request: %w", err)
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("x-api-key", c.APIKey)
	httpReq.Header.Set("anthropic-version", "2023-06-01")

	resp, err := c.HTTP.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("send request: %w", err)
	}
	defer resp.Body.Close()

	respData, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("read response: %w", err)
	}

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("API error %d: %s", resp.StatusCode, string(respData))
	}

	slog.Info("chat response", "body", string(respData))

	var cr anthropicChatResponse
	if err := json.Unmarshal(respData, &cr); err != nil {
		return nil, fmt.Errorf("parse response: %w", err)
	}
	if cr.Error != nil {
		return nil, fmt.Errorf("API error: %s (%s)", cr.Error.Message, cr.Error.Type)
	}

	result := &ChatResponse{StopReason: cr.StopReason}
	for _, raw := range cr.Content {
		// Store raw block for passing back to API (needed for thinking mode)
		var rawMap map[string]interface{}
		json.Unmarshal(raw, &rawMap)
		result.AllContent = append(result.AllContent, rawMap)

		// Identify the block type
		var typeCheck struct{ Type string }
		if err := json.Unmarshal(raw, &typeCheck); err != nil {
			continue
		}
		switch typeCheck.Type {
		case "text":
			var tb anthropicTextBlock
			if json.Unmarshal(raw, &tb) == nil {
				result.Text += tb.Text
			}
		case "tool_use":
			var tu anthropicToolUseBlock
			if json.Unmarshal(raw, &tu) == nil {
				result.ToolUses = append(result.ToolUses, ToolUseBlock{
					Type:  tu.Type,
					ID:    tu.ID,
					Name:  tu.Name,
					Input: tu.Input,
				})
			}
		}
	}

	return result, nil
}

// CreateEmbedding generates an embedding vector (OpenAI format).
func (c *Client) CreateEmbedding(model, input string) ([]float64, error) {
	url := c.EmbedURL + "/embeddings"

	req := EmbeddingRequest{Model: model, Input: input}
	data, err := json.Marshal(req)
	if err != nil {
		return nil, fmt.Errorf("marshal request: %w", err)
	}

	slog.Info("embed request", "url", url)

	httpReq, err := http.NewRequest("POST", url, bytes.NewReader(data))
	if err != nil {
		return nil, fmt.Errorf("create request: %w", err)
	}
	httpReq.Header.Set("Content-Type", "application/json")
	embedKey := c.EmbedAPIKey
	if embedKey == "" {
		embedKey = c.APIKey
	}
	httpReq.Header.Set("Authorization", "Bearer "+embedKey)

	resp, err := c.HTTP.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("send request: %w", err)
	}
	defer resp.Body.Close()

	respData, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("read response: %w", err)
	}

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("API error %d: %s", resp.StatusCode, string(respData))
	}

	var er embeddingResponse
	if err := json.Unmarshal(respData, &er); err != nil {
		return nil, fmt.Errorf("parse response: %w", err)
	}
	if er.Error != nil {
		return nil, fmt.Errorf("API error: %s (%s)", er.Error.Message, er.Error.Type)
	}
	if len(er.Data) == 0 {
		return nil, fmt.Errorf("no embedding data in response")
	}

	return er.Data[0].Embedding, nil
}
