package store

import "time"

type Skill struct {
	ID           int64     `json:"id"`
	Name         string    `json:"name"`
	Description  string    `json:"description"`
	TriggerType  string    `json:"trigger_type"`
	TriggerConfig string   `json:"trigger_config"`
	ActionPrompt string    `json:"action_prompt"`
	CreatedBy    string    `json:"created_by"`
	ChatID       string    `json:"chat_id"`
	CreatedAt    time.Time `json:"created_at"`
	UpdatedAt    time.Time `json:"updated_at"`
}

type MemoryFact struct {
	FactID   string                 `json:"fact_id"`
	Content  string                 `json:"content"`
	Metadata map[string]interface{} `json:"metadata,omitempty"`
}
