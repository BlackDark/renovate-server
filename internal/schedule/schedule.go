// Package schedule fires periodic full-discovery renovate runs per platform.
package schedule

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/robfig/cron/v3"

	"github.com/BlackDark/renovate-server/internal/config"
)

// Runner owns one cron scheduler per platform (each with its own
// timezone).
type Runner struct {
	crons []*cron.Cron
	log   *slog.Logger
}

// New returns an empty Runner; register platforms with AddPlatform.
func New(log *slog.Logger) *Runner {
	return &Runner{log: log}
}

// AddPlatform registers the platform's crontabs; each firing invokes job.
// Overlapping firings are safe: the dispatcher coalesces per repo.
func (r *Runner) AddPlatform(sched config.Schedule, job func()) error {
	if len(sched.Crontabs) == 0 {
		return nil
	}
	loc := time.UTC
	if sched.Timezone != "" {
		var err error
		loc, err = time.LoadLocation(sched.Timezone)
		if err != nil {
			return fmt.Errorf("invalid timezone %q: %w", sched.Timezone, err)
		}
	}
	c := cron.New(cron.WithLocation(loc))
	for _, tab := range sched.Crontabs {
		if _, err := c.AddFunc(tab, job); err != nil {
			return fmt.Errorf("invalid crontab %q: %w", tab, err)
		}
	}
	r.crons = append(r.crons, c)
	return nil
}

// Start begins firing all registered schedules.
func (r *Runner) Start() {
	for _, c := range r.crons {
		c.Start()
	}
}

// Stop halts scheduling; running jobs drain in the background.
func (r *Runner) Stop() context.Context {
	ctx := context.Background()
	for _, c := range r.crons {
		ctx = c.Stop()
	}
	return ctx
}
