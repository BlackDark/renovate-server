// Package noop provides a shadow-mode executor: it accepts runs, logs the
// decision and does nothing. Point rules at it to validate webhook parsing
// and dispatch against production traffic before real runs are enabled;
// decisions show up in /api/v1/runs.
package noop

import (
	"context"
	"log/slog"

	"github.com/BlackDark/renovate-server/internal/config"
	"github.com/BlackDark/renovate-server/internal/executor"
)

// Executor implements executor.Executor without side effects.
type Executor struct {
	name string
	log  *slog.Logger
}

// New builds a noop Executor from its config section.
func New(cfg config.Executor, log *slog.Logger) *Executor {
	return &Executor{name: cfg.Name, log: log.With("executor", cfg.Name)}
}

// Name returns the executor's configured name.
func (e *Executor) Name() string { return e.name }

// Run logs the would-be run and returns success immediately.
func (e *Executor) Run(_ context.Context, spec executor.RunSpec) error {
	e.log.Info("noop executor: run skipped", "repo", spec.Repo.Key(), "reason", string(spec.Reason))
	return nil
}
