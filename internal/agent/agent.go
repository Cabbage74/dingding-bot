package agent

import (
	"fmt"
	"log/slog"
	"strings"

	"github.com/cabbage/dingding-bot/internal/llm"
	"github.com/cabbage/dingding-bot/internal/memory"
	"github.com/cabbage/dingding-bot/internal/skill"
	dingtalk "github.com/cabbage/dingding-bot/internal/dingtalk"
)

// Agent is the core bot agent that orchestrates all subsystems.
type Agent struct {
	llm            *llm.Client
	llmModel       string
	embModel       string
	chroma         *memory.ChromaClient
	contextMgr     *memory.ContextManager
	extractor      *memory.Extractor
	skills         *skill.Registry
	dtClient       *dingtalk.Client
	onSkillCreated func()    // called when a skill is created (to reload scheduler)
	onMessage      func()    // called on each processed message (for stats)
}

// New creates a new Agent.
func New(
	llmClient *llm.Client,
	llmModel, embModel string,
	chromaClient *memory.ChromaClient,
	contextMgr *memory.ContextManager,
	extractor *memory.Extractor,
	skillReg *skill.Registry,
	dtClient *dingtalk.Client,
) *Agent {
	return &Agent{
		llm:        llmClient,
		llmModel:   llmModel,
		embModel:   embModel,
		chroma:     chromaClient,
		contextMgr: contextMgr,
		extractor:  extractor,
		skills:     skillReg,
		dtClient:   dtClient,
	}
}

// SetOnSkillCreated sets a callback for when a new skill is created.
func (a *Agent) SetOnSkillCreated(fn func()) {
	a.onSkillCreated = fn
}

// SetOnMessage sets a callback called on each processed message.
func (a *Agent) SetOnMessage(fn func()) {
	a.onMessage = fn
}

// ProcessMessage handles a single incoming message event.
func (a *Agent) ProcessMessage(event *dingtalk.Event) error {
	chatID := event.ChatID
	userName := event.SenderNick
	if userName == "" {
		userName = event.SenderID
	}

	content := ""
	if event.Text.Content != "" {
		content = event.Text.Content
	}
	if content == "" {
		return nil // ignore non-text messages
	}

	slog.Info("processing message", "chat_id", chatID, "user", userName, "content", content)

	if a.onMessage != nil {
		a.onMessage()
	}

	// 0. Handle commands
	if done := a.handleCommands(chatID, userName, content, event.SessionWebhook); done {
		return nil
	}

	// 1. Add user message to context
	userMsg := llm.Message{Role: "user", Content: fmt.Sprintf("%s: %s", userName, content)}
	a.contextMgr.AddMessage(chatID, userMsg)

	// 2. Get context messages and summary
	contextMsgs := a.contextMgr.GetMessages(chatID)
	contextSummary := a.contextMgr.GetSummary(chatID)

	// 3. Search relevant memories
	memories := a.searchMemories(content)

	// 4. Check for skill triggers
	skillCtx := a.matchSkills(event)

	// 5. Build system prompt and messages
	systemPrompt := a.buildSystemPrompt(memories, skillCtx, contextSummary)

	// 6. Call LLM
	resp, err := a.llm.ChatCompletion(&llm.ChatRequest{
		Model:       a.llmModel,
		Messages:    contextMsgs,
		System:      systemPrompt,
		MaxTokens:   2000,
		Temperature: 0.7,
		Tools:       a.getTools(),
	})
	if err != nil {
		slog.Error("llm call failed", "error", err)
		return fmt.Errorf("llm call: %w", err)
	}

	// Handle tool calls
	finalText := resp.Text
	if len(resp.ToolUses) > 0 {
		// Execute tools and get final response
		finalText = a.executeTools(contextMsgs, systemPrompt, resp)
	}

	// 7. Add assistant response to context
	assistantMsg := llm.Message{Role: "assistant", Content: finalText}
	a.contextMgr.AddMessage(chatID, assistantMsg)

	// 8. Send reply
	if event.SessionWebhook != "" {
		if err := a.dtClient.ReplyViaWebhook(event.SessionWebhook, finalText); err != nil {
			slog.Error("webhook reply failed", "error", err)
		}
	} else {
		if err := a.dtClient.ReplyText(chatID, finalText); err != nil {
			slog.Error("send reply failed", "error", err)
		}
	}

	// 9. Async extract memories
	go a.extractMemories(chatID, content, finalText)

	return nil
}

func (a *Agent) searchMemories(query string) []memory.SearchResult {
	emb, err := a.llm.CreateEmbedding(a.embModel, query)
	if err != nil {
		slog.Warn("embedding failed", "error", err)
		return nil
	}

	results, err := a.chroma.SearchMemory(emb, 5)
	if err != nil {
		slog.Warn("memory search failed", "error", err)
		return nil
	}
	return results
}

func (a *Agent) matchSkills(event *dingtalk.Event) *skill.ExecutionContext {
	content := event.Text.Content

	// Check keyword/mention skills
	matched := a.skills.Match(content)
	if len(matched) > 0 {
		return &skill.ExecutionContext{
			Skills: matched,
		}
	}
	return nil
}

func (a *Agent) buildSystemPrompt(memories []memory.SearchResult, skillCtx *skill.ExecutionContext, summary string) string {
	sb := &strings.Builder{}
	sb.WriteString(SystemPrompt)

	// Inject context summary
	if summary != "" {
		sb.WriteString("\n\n## 先前的对话摘要\n")
		sb.WriteString(summary)
	}

	// Inject memories
	if len(memories) > 0 {
		sb.WriteString("\n\n## 相关的历史记忆\n")
		for _, m := range memories {
			sb.WriteString(fmt.Sprintf("- %s\n", m.Document))
		}
	}

	// Inject skill context
	if skillCtx != nil && len(skillCtx.Skills) > 0 {
		sb.WriteString("\n## 触发的技能\n")
		for _, s := range skillCtx.Skills {
			sb.WriteString(fmt.Sprintf("- %s\n", s.ActionPrompt))
		}
	}

	return sb.String()
}

func (a *Agent) extractMemories(chatID, userMsg, assistantMsg string) {
	conversation := []llm.Message{
		{Role: "user", Content: userMsg},
		{Role: "assistant", Content: assistantMsg},
	}

	facts, err := a.extractor.Extract(conversation)
	if err != nil {
		slog.Warn("memory extraction failed", "error", err)
		return
	}

	for _, fact := range facts {
		switch fact.Action {
		case "remember":
			emb, err := a.llm.CreateEmbedding(a.embModel, fact.Content)
			if err != nil {
				slog.Warn("embedding for memory failed", "error", err)
				continue
			}
			if fact.Metadata == nil {
				fact.Metadata = make(map[string]interface{})
			}
			fact.Metadata["chat_id"] = chatID
			if err := a.chroma.AddMemory(fact.FactID, fact.Content, emb, fact.Metadata); err != nil {
				slog.Error("store memory failed", "error", err)
			} else {
				slog.Info("stored memory", "fact_id", fact.FactID, "content", fact.Content)
			}
		case "forget":
			if err := a.chroma.DeleteMemory(fact.FactID); err != nil {
				slog.Warn("delete memory failed", "error", err, "fact_id", fact.FactID)
			} else {
				slog.Info("deleted memory", "fact_id", fact.FactID)
			}
		case "update":
			emb, err := a.llm.CreateEmbedding(a.embModel, fact.Content)
			if err != nil {
				slog.Warn("embedding for update failed", "error", err)
				continue
			}
			// Delete old and add new
			a.chroma.DeleteMemory(fact.FactID)
			if fact.Metadata == nil {
				fact.Metadata = make(map[string]interface{})
			}
			fact.Metadata["chat_id"] = chatID
			if err := a.chroma.AddMemory(fact.FactID, fact.Content, emb, fact.Metadata); err != nil {
				slog.Error("update memory failed", "error", err)
			} else {
				slog.Info("updated memory", "fact_id", fact.FactID)
			}
		}
	}
}

// GenerateSkill is used for natural language skill creation.
func (a *Agent) GenerateSkill(userPrompt string) (*skill.SkillDef, error) {
	resp, err := a.llm.ChatCompletion(&llm.ChatRequest{
		Model:       a.llmModel,
		Messages:    []llm.Message{{Role: "user", Content: userPrompt}},
		System:      skillCreationPrompt,
		MaxTokens:   500,
		Temperature: 0.3,
	})
	if err != nil {
		return nil, fmt.Errorf("generate skill: %w", err)
	}

	return skill.ParseSkillDef(resp.Text)
}

// handleSkillCreation parses skill creation intent and creates the skill.
func (a *Agent) handleSkillCreation(chatID, userName, content, webhook string) error {
	def, err := a.GenerateSkill(content)
	if err != nil {
		slog.Error("generate skill failed", "error", err)
		if webhook != "" {
			a.dtClient.ReplyViaWebhook(webhook, "创建 skill 失败，请再试一次。")
		}
		return fmt.Errorf("generate skill: %w", err)
	}

	s, err := a.skills.Create(def, userName, chatID)
	if err != nil {
		slog.Error("create skill failed", "error", err)
		return fmt.Errorf("create skill: %w", err)
	}

	slog.Info("skill created", "name", s.Name, "trigger", s.TriggerType, "config", s.TriggerConfig, "chat_id", chatID)

	if a.onSkillCreated != nil {
		a.onSkillCreated()
	}

	reply := fmt.Sprintf("技能「%s」创建成功！触发方式：%s，配置：%s", s.Name, s.TriggerType, s.TriggerConfig)
	if webhook != "" {
		a.dtClient.ReplyViaWebhook(webhook, reply)
	}
	return nil
}

func (a *Agent) handleCommands(chatID, userName, content, webhook string) bool {
	reply := func(msg string) {
		if webhook != "" {
			a.dtClient.ReplyViaWebhook(webhook, msg)
		}
	}

	// Skill creation
	if strings.Contains(content, "创建") && (strings.Contains(content, "skill") || strings.Contains(content, "技能") || strings.Contains(content, "定时")) {
		a.handleSkillCreation(chatID, userName, content, webhook)
		return true
	}

	// List skills
	if strings.Contains(content, "列出") && (strings.Contains(content, "skill") || strings.Contains(content, "技能")) ||
		strings.Contains(content, "技能列表") || strings.Contains(content, "有哪些技能") {
		skills, err := a.skills.List()
		if err != nil {
			reply("获取技能列表失败")
			return true
		}
		if len(skills) == 0 {
			reply("目前没有任何技能。试试说「帮我创建一个每天早上8点推送天气预报的skill」")
			return true
		}
		var sb strings.Builder
		sb.WriteString("当前技能列表：\n")
		for _, s := range skills {
			if s.ChatID == "" || s.ChatID == chatID {
				fmt.Fprintf(&sb, "• %s [%s] %s\n", s.Name, s.TriggerType, s.TriggerConfig)
			}
		}
		reply(sb.String())
		return true
	}

	// Delete skill
	if strings.Contains(content, "删除") && (strings.Contains(content, "skill") || strings.Contains(content, "技能")) {
		// Try to find skill by name in the content
		skills, err := a.skills.List()
		if err != nil {
			reply("获取技能列表失败")
			return true
		}
		var target string
		for _, s := range skills {
			if s.ChatID == chatID && strings.Contains(content, s.Name) {
				target = s.Name
				break
			}
		}
		if target == "" {
			reply("没有找到匹配的技能。用「列出技能」查看当前技能。")
			return true
		}
		for _, s := range skills {
			if s.Name == target && s.ChatID == chatID {
				if err := a.skills.Delete(s.ID); err != nil {
					reply("删除失败")
					return true
				}
				if a.onSkillCreated != nil {
					a.onSkillCreated()
				}
				reply(fmt.Sprintf("技能「%s」已删除", s.Name))
				return true
			}
		}
	}

	// Delete memory
	if strings.Contains(content, "忘记") || strings.Contains(content, "清除") ||
		(strings.Contains(content, "删除") && (strings.Contains(content, "记忆") || strings.Contains(content, "记"))) {
		emb, err := a.llm.CreateEmbedding(a.embModel, content)
		if err != nil {
			reply("检索记忆失败")
			return true
		}
		results, err := a.chroma.SearchMemory(emb, 5)
		if err != nil || len(results) == 0 {
			reply("没有找到相关记忆")
			return true
		}
		// Delete the closest match
		deleted := results[0]
		if err := a.chroma.DeleteMemory(deleted.ID); err != nil {
			reply("删除失败")
			return true
		}
		reply(fmt.Sprintf("已忘记：%s", deleted.Document))
		return true
	}

	// System status
	if strings.Contains(content, "系统状态") || strings.Contains(content, "运行状态") || strings.Contains(content, "bot状态") {
		skills, _ := a.skills.List()
		memories := a.chroma.ListMemories(1)
		memCount := len(memories)
		reply(fmt.Sprintf("🤖 尼格运行正常\n• 技能数：%d\n• 记忆数：%d",
			len(skills), memCount))
		return true
	}

	// Show memories (only if no deletion intent)
	if strings.Contains(content, "记忆") || strings.Contains(content, "记得什么") || strings.Contains(content, "记住什么") || strings.Contains(content, "知道什么") {
		emb, err := a.llm.CreateEmbedding(a.embModel, content)
		if err != nil {
			reply("检索记忆失败")
			return true
		}
		results, err := a.chroma.SearchMemory(emb, 10)
		if err != nil {
			reply("检索记忆失败")
			return true
		}
		if len(results) == 0 {
			reply("目前没有任何记忆。多聊聊天我就能记住啦！")
			return true
		}
		var sb strings.Builder
		sb.WriteString("我记得这些：\n")
		for _, r := range results {
			fmt.Fprintf(&sb, "• %s\n", r.Document)
		}
		reply(sb.String())
		return true
	}

	return false
}

const skillCreationPrompt = `You are a skill configurator. Parse the user's request into a skill definition.

Respond with ONLY a JSON object:
{
  "name": "short skill name",
  "description": "what this skill does",
  "trigger_type": "cron" or "keyword" or "mention",
  "trigger_config": "cron expression" or "comma-separated keywords",
  "action_prompt": "what the bot should do when triggered"
}

Rules:
- trigger_type "cron": trigger_config should be a standard 5-field cron expression (minute hour day month weekday). IMPORTANT: all time must be in Asia/Shanghai (UTC+8).
- trigger_type "keyword": trigger_config should be comma-separated keywords
- trigger_type "mention": trigger_config can be empty
- action_prompt should be a clear instruction in Chinese pointing to what content to generate and send`
