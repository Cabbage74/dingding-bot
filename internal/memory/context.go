package memory

import (
	"sync"
	"time"

	"github.com/cabbage/dingding-bot/internal/llm"
)

// Context holds a group chat's conversation context.
type Context struct {
	ChatID    string
	Messages  []llm.Message
	Summary   string
	lastAccess time.Time
}

// IsExpired returns true if the context hasn't been accessed within ttl.
func (c *Context) IsExpired(ttl time.Duration) bool {
	return time.Since(c.lastAccess) > ttl
}

// AddMessage appends a message and trims to maxMessages, returning overflow.
func (c *Context) AddMessage(msg llm.Message, maxMessages int) []llm.Message {
	c.lastAccess = time.Now()
	c.Messages = append(c.Messages, msg)
	if len(c.Messages) > maxMessages {
		overflow := c.Messages[0 : len(c.Messages)-maxMessages]
		c.Messages = c.Messages[len(c.Messages)-maxMessages:]
		return overflow
	}
	return nil
}

// ContextManager manages per-group chat contexts.
type ContextManager struct {
	mu          sync.RWMutex
	contexts    map[string]*Context
	maxMessages int
	ttl         time.Duration
	llmClient   *llm.Client
	llmModel    string
}

// NewContextManager creates a new context manager.
func NewContextManager(maxMessages int, ttl time.Duration) *ContextManager {
	return &ContextManager{
		contexts:    make(map[string]*Context),
		maxMessages: maxMessages,
		ttl:         ttl,
	}
}

// SetLLMClient sets the LLM client for auto-summarization.
func (m *ContextManager) SetLLMClient(client *llm.Client, model string) {
	m.llmClient = client
	m.llmModel = model
}

// Get returns the context for a chat, creating one if it doesn't exist.
func (m *ContextManager) Get(chatID string) *Context {
	m.mu.Lock()
	defer m.mu.Unlock()

	ctx, ok := m.contexts[chatID]
	if ok && !ctx.IsExpired(m.ttl) {
		return ctx
	}

	ctx = &Context{
		ChatID:     chatID,
		Messages:   make([]llm.Message, 0, m.maxMessages),
		lastAccess: time.Now(),
	}
	m.contexts[chatID] = ctx
	return ctx
}

// AddMessage adds a message to the chat's context.
func (m *ContextManager) AddMessage(chatID string, msg llm.Message) {
	m.mu.Lock()
	ctx, ok := m.contexts[chatID]
	if !ok || ctx.IsExpired(m.ttl) {
		ctx = &Context{
			ChatID:     chatID,
			Messages:   make([]llm.Message, 0, m.maxMessages),
			lastAccess: time.Now(),
		}
		m.contexts[chatID] = ctx
	}
	m.mu.Unlock()

	overflow := ctx.AddMessage(msg, m.maxMessages)
	if len(overflow) > 0 && m.llmClient != nil {
		m.summarize(ctx, overflow)
	}
}

func (m *ContextManager) summarize(ctx *Context, overflow []llm.Message) {
	resp, err := m.llmClient.ChatCompletion(&llm.ChatRequest{
		Model:    m.llmModel,
		Messages: overflow,
		System:   "Summarize the following conversation history concisely in 1-2 sentences in Chinese. Keep key facts, decisions, and context.",
	})
	if err != nil {
		return
	}

	if ctx.Summary != "" {
		ctx.Summary = ctx.Summary + "\n" + resp.Text
	} else {
		ctx.Summary = resp.Text
	}
}

// GetMessages returns the conversation messages for a chat.
func (m *ContextManager) GetMessages(chatID string) []llm.Message {
	m.mu.RLock()
	ctx, ok := m.contexts[chatID]
	m.mu.RUnlock()

	if !ok || ctx.IsExpired(m.ttl) {
		return nil
	}

	return ctx.Messages
}

// GetSummary returns the conversation summary for a chat.
func (m *ContextManager) GetSummary(chatID string) string {
	m.mu.RLock()
	ctx, ok := m.contexts[chatID]
	m.mu.RUnlock()

	if !ok || ctx.IsExpired(m.ttl) {
		return ""
	}
	return ctx.Summary
}

// Cleanup removes expired contexts.
func (m *ContextManager) Cleanup() {
	m.mu.Lock()
	defer m.mu.Unlock()

	for id, ctx := range m.contexts {
		if ctx.IsExpired(m.ttl) {
			delete(m.contexts, id)
		}
	}
}
