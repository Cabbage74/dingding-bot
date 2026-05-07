package memory

import (
	"encoding/json"
	"fmt"

	"github.com/cabbage/dingding-bot/internal/llm"
)

// Extractor uses LLM to extract structured facts from conversations.
type Extractor struct {
	client *llm.Client
	model  string
}

// ExtractedMemory represents a fact extracted from conversation or an update operation.
type ExtractedMemory struct {
	FactID   string                 `json:"fact_id"`
	Action   string                 `json:"action"`   // "remember", "forget", "update"
	Content  string                 `json:"content"`
	Metadata map[string]interface{} `json:"metadata"`
}

// NewExtractor creates a new memory extractor.
func NewExtractor(client *llm.Client, model string) *Extractor {
	return &Extractor{client: client, model: model}
}

// Extract analyzes a conversation turn and extracts/updates memories.
func (e *Extractor) Extract(conversation []llm.Message) ([]ExtractedMemory, error) {
	resp, err := e.client.ChatCompletion(&llm.ChatRequest{
		Model:       e.model,
		Messages:    conversation,
		System:      extractSystemPrompt,
		Temperature: 0.3,
		MaxTokens:   1000,
	})
	if err != nil {
		return nil, fmt.Errorf("extract memories: %w", err)
	}

	var memories []ExtractedMemory
	if err := json.Unmarshal([]byte(resp.Text), &memories); err != nil {
		// If response is not valid JSON, return empty (no memories extracted)
		return nil, nil
	}
	return memories, nil
}

const extractSystemPrompt = `You are a memory manager for a group chat AI assistant. Your task is to analyze the conversation and extract structured facts worth remembering.

For each fact, decide if it should be:
- "remember": new information to store
- "update": update an existing fact (provide fact_id)
- "forget": remove an outdated fact (provide fact_id)

Respond with a JSON array. Each entry should have:
- "action": "remember", "update", or "forget"
- "fact_id": a unique ID for the fact (for update/forget, reference the existing ID)
- "content": the fact content (1-2 sentences, specific and concise)
- "metadata": additional context (e.g., {"source": "user_name", "category": "preference"})

Only extract information that is:
1. Likely to be useful in future conversations
2. Factual and specific (preferences, decisions, personal info, plans)
3. Not already obvious from the immediate context

If there is nothing worth remembering, respond with an empty array: []

Use Chinese for the content if the conversation is in Chinese.`
