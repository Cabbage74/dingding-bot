package skill

import (
	"log/slog"

	store "github.com/cabbage/dingding-bot/internal/store"

	"github.com/robfig/cron/v3"
)

// Executor handles executing skills.
type Executor struct {
	registry *Registry
	cron     *cron.Cron
	handler  SkillHandler
}

// SkillHandler is called when a cron skill fires.
type SkillHandler func(skill store.Skill) error

// NewExecutor creates a new skill executor.
func NewExecutor(reg *Registry, handler SkillHandler) *Executor {
	return &Executor{
		registry: reg,
		cron:     cron.New(),
		handler:  handler,
	}
}

// Start loads all cron skills and starts the scheduler.
func (e *Executor) Start() error {
	skills, err := e.registry.GetCronSkills()
	if err != nil {
		return err
	}

	for _, s := range skills {
		e.addCron(s)
	}

	e.cron.Start()
	slog.Info("skill scheduler started", "cron_skills", len(skills))
	return nil
}

// Stop stops the scheduler.
func (e *Executor) Stop() {
	e.cron.Stop()
}

// Reload re-reads all cron skills from the database.
func (e *Executor) Reload() {
	// Remove all existing cron entries
	for _, entry := range e.cron.Entries() {
		e.cron.Remove(entry.ID)
	}

	skills, err := e.registry.GetCronSkills()
	if err != nil {
		slog.Error("reload cron skills", "error", err)
		return
	}

	for _, s := range skills {
		e.addCron(s)
	}
	slog.Info("reloaded cron skills", "count", len(skills))
}

// addCron adds a single cron skill to the scheduler.
func (e *Executor) addCron(s store.Skill) {
	skill := s
	_, err := e.cron.AddFunc(skill.TriggerConfig, func() {
		slog.Info("cron skill triggered", "name", skill.Name, "id", skill.ID)
		if err := e.handler(skill); err != nil {
			slog.Error("execute cron skill", "name", skill.Name, "error", err)
		}
	})
	if err != nil {
		slog.Error("add cron job", "name", s.Name, "cron", s.TriggerConfig, "error", err)
	}
}
