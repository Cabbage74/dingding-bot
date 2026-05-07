package skill

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/cabbage/dingding-bot/internal/store"
)

// SkillDef is used when creating/parsing skills.
type SkillDef struct {
	Name          string `json:"name"`
	Description   string `json:"description"`
	TriggerType   string `json:"trigger_type"`
	TriggerConfig string `json:"trigger_config"`
	ActionPrompt  string `json:"action_prompt"`
}

// ExecutionContext holds matched skills for the current message.
type ExecutionContext struct {
	Skills []store.Skill
}

// ParseSkillDef parses a JSON string into a SkillDef.
func ParseSkillDef(jsonStr string) (*SkillDef, error) {
	// Extract JSON from markdown code blocks if present
	jsonStr = strings.TrimSpace(jsonStr)
	if strings.HasPrefix(jsonStr, "```") {
		lines := strings.Split(jsonStr, "\n")
		if len(lines) > 2 {
			jsonStr = strings.Join(lines[1:len(lines)-1], "\n")
		}
	}

	var def SkillDef
	if err := json.Unmarshal([]byte(jsonStr), &def); err != nil {
		return nil, fmt.Errorf("parse skill def: %w", err)
	}
	return &def, nil
}

// Registry manages skills in SQLite.
type Registry struct {
	db *sql.DB
}

// NewRegistry creates a new skill registry.
func NewRegistry(db *sql.DB) *Registry {
	return &Registry{db: db}
}

// Create adds a new skill.
func (r *Registry) Create(def *SkillDef, createdBy, chatID string) (*store.Skill, error) {
	if def.TriggerType == "" {
		def.TriggerType = "mention"
	}

	now := time.Now()
	result, err := r.db.Exec(
		`INSERT INTO skills (name, description, trigger_type, trigger_config, action_prompt, created_by, chat_id, created_at, updated_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		def.Name, def.Description, def.TriggerType, def.TriggerConfig, def.ActionPrompt, createdBy, chatID, now, now,
	)
	if err != nil {
		return nil, fmt.Errorf("create skill: %w", err)
	}

	id, _ := result.LastInsertId()
	return &store.Skill{
		ID:           id,
		Name:         def.Name,
		Description:  def.Description,
		TriggerType:  def.TriggerType,
		TriggerConfig: def.TriggerConfig,
		ActionPrompt: def.ActionPrompt,
		CreatedBy:    createdBy,
		ChatID:       chatID,
		CreatedAt:    now,
		UpdatedAt:    now,
	}, nil
}

const skillColumns = `id, name, description, trigger_type, trigger_config, action_prompt, created_by, chat_id, created_at, updated_at`

func scanSkill(s *store.Skill, row interface{ Scan(...interface{}) error }) error {
	return row.Scan(&s.ID, &s.Name, &s.Description, &s.TriggerType, &s.TriggerConfig, &s.ActionPrompt, &s.CreatedBy, &s.ChatID, &s.CreatedAt, &s.UpdatedAt)
}

// Get retrieves a skill by ID.
func (r *Registry) Get(id int64) (*store.Skill, error) {
	var s store.Skill
	err := scanSkill(&s, r.db.QueryRow(
		`SELECT `+skillColumns+` FROM skills WHERE id = ?`, id,
	))
	if err != nil {
		return nil, err
	}
	return &s, nil
}

// List returns all skills.
func (r *Registry) List() ([]store.Skill, error) {
	rows, err := r.db.Query(
		`SELECT ` + skillColumns + ` FROM skills ORDER BY id DESC`,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var skills []store.Skill
	for rows.Next() {
		var s store.Skill
		if err := scanSkill(&s, rows); err != nil {
			return nil, err
		}
		skills = append(skills, s)
	}
	return skills, nil
}

// Delete removes a skill by ID.
func (r *Registry) Delete(id int64) error {
	_, err := r.db.Exec("DELETE FROM skills WHERE id = ?", id)
	return err
}

// Match finds skills that match the given message content.
func (r *Registry) Match(content string) []store.Skill {
	skills, err := r.List()
	if err != nil {
		return nil
	}

	var matched []store.Skill
	lowerContent := strings.ToLower(content)
	for _, s := range skills {
		switch s.TriggerType {
		case "keyword":
			keywords := strings.Split(s.TriggerConfig, ",")
			for _, kw := range keywords {
				if strings.Contains(lowerContent, strings.ToLower(strings.TrimSpace(kw))) {
					matched = append(matched, s)
					break
				}
			}
		case "mention":
			// Mention skills always trigger when the bot is called
			matched = append(matched, s)
		}
	}
	return matched
}

// SaveMessage stores a chat message for later summarization.
func (r *Registry) SaveMessage(chatID, userName, content string) error {
	_, err := r.db.Exec(
		`INSERT INTO messages (chat_id, user_name, content) VALUES (?, ?, ?)`,
		chatID, userName, content,
	)
	return err
}

// GetWeeklyMessages returns messages from the past 7 days for a chat.
func (r *Registry) GetWeeklyMessages(chatID string) ([]string, error) {
	rows, err := r.db.Query(
		`SELECT user_name, content FROM messages WHERE chat_id = ? AND created_at > datetime('now', '-7 days') ORDER BY created_at ASC`,
		chatID,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var msgs []string
	for rows.Next() {
		var user, content string
		if err := rows.Scan(&user, &content); err != nil {
			continue
		}
		msgs = append(msgs, fmt.Sprintf("%s: %s", user, content))
	}
	return msgs, nil
}

// GetCronSkills returns all cron-triggered skills.
func (r *Registry) GetCronSkills() ([]store.Skill, error) {
	rows, err := r.db.Query(
		`SELECT ` + skillColumns + ` FROM skills WHERE trigger_type = 'cron'`,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var skills []store.Skill
	for rows.Next() {
		var s store.Skill
		if err := scanSkill(&s, rows); err != nil {
			return nil, err
		}
		skills = append(skills, s)
	}
	return skills, nil
}
