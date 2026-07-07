// Package gitlabci runs renovate by triggering a pipeline in a central
// GitLab project and polling it to completion.
package gitlabci

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"text/template"
	"time"

	gogitlab "gitlab.com/gitlab-org/api/client-go"

	"github.com/BlackDark/renovate-server/internal/config"
	"github.com/BlackDark/renovate-server/internal/executor"
)

const maxConsecutivePollErrors = 5

type Executor struct {
	name         string
	client       *gogitlab.Client
	project      string
	ref          string
	triggerToken string
	variables    map[string]*template.Template
	pollInterval time.Duration
	log          *slog.Logger
}

// templateData is the render context for variable templates.
type templateData struct {
	Repo     string
	Platform string
	Reason   string
}

func New(cfg config.Executor, client *gogitlab.Client, log *slog.Logger) (*Executor, error) {
	vars := make(map[string]*template.Template, len(cfg.Variables))
	for k, v := range cfg.Variables {
		tmpl, err := template.New(k).Option("missingkey=error").Parse(v)
		if err != nil {
			return nil, fmt.Errorf("executor %q: variable %q: %w", cfg.Name, k, err)
		}
		vars[k] = tmpl
	}
	return &Executor{
		name:         cfg.Name,
		client:       client,
		project:      cfg.Project,
		ref:          cfg.Ref,
		triggerToken: cfg.TriggerToken,
		variables:    vars,
		pollInterval: cfg.PollInterval,
		log:          log.With("executor", cfg.Name),
	}, nil
}

func (e *Executor) Name() string { return e.name }

func (e *Executor) Run(ctx context.Context, spec executor.RunSpec) error {
	data := templateData{
		Repo:     spec.Repo.FullName,
		Platform: spec.Repo.Platform,
		Reason:   string(spec.Reason),
	}
	vars := make(map[string]string, len(e.variables))
	for k, tmpl := range e.variables {
		var sb strings.Builder
		if err := tmpl.Execute(&sb, data); err != nil {
			return fmt.Errorf("render variable %q: %w", k, err)
		}
		vars[k] = sb.String()
	}

	pipeline, _, err := e.client.PipelineTriggers.RunPipelineTrigger(e.project,
		&gogitlab.RunPipelineTriggerOptions{
			Ref:       gogitlab.Ptr(e.ref),
			Token:     gogitlab.Ptr(e.triggerToken),
			Variables: vars,
		}, gogitlab.WithContext(ctx))
	if err != nil {
		return fmt.Errorf("trigger pipeline in %q: %w", e.project, err)
	}
	log := e.log.With("repo", spec.Repo.Key(), "pipeline", pipeline.ID)
	log.Info("pipeline triggered")

	ticker := time.NewTicker(e.pollInterval)
	defer ticker.Stop()
	pollErrors := 0
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
		}

		p, _, err := e.client.Pipelines.GetPipeline(e.project, pipeline.ID, gogitlab.WithContext(ctx))
		if err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			pollErrors++
			if pollErrors >= maxConsecutivePollErrors {
				return fmt.Errorf("poll pipeline %d: %w", pipeline.ID, err)
			}
			log.Warn("pipeline poll failed, retrying", "error", err, "attempt", pollErrors)
			continue
		}
		pollErrors = 0

		switch p.Status {
		case "success":
			return nil
		case "failed", "canceled", "skipped":
			return fmt.Errorf("pipeline %d finished with status %q", pipeline.ID, p.Status)
		default:
			// created|waiting_for_resource|preparing|pending|running|manual|scheduled: keep polling
		}
	}
}
