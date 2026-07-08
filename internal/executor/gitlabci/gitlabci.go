// Package gitlabci runs renovate by triggering a pipeline in a central
// GitLab project and polling it to completion.
package gitlabci

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"
	"text/template"
	"time"

	gogitlab "gitlab.com/gitlab-org/api/client-go"

	"github.com/BlackDark/renovate-server/internal/config"
	"github.com/BlackDark/renovate-server/internal/executor"
	"github.com/BlackDark/renovate-server/internal/platform"
)

const maxConsecutivePollErrors = 5

// HandleStore persists active-run state so pipelines can be re-adopted
// after a restart. Satisfied by store.Store; may be nil (no persistence).
type HandleStore interface {
	SaveRunHandle(key, data string)
	LoadRunHandles() map[string]string
	DeleteRunHandle(key string)
}

// runHandle is the JSON payload persisted per active pipeline run.
type runHandle struct {
	Executor   string `json:"executor"`
	Platform   string `json:"platform"`
	Repo       string `json:"repo"`
	Project    string `json:"project"`
	PipelineID int64  `json:"pipelineID"`
}

// Executor triggers pipelines in a central GitLab project.
type Executor struct {
	name         string
	client       *gogitlab.Client
	project      string
	ref          string
	triggerToken string
	variables    map[string]*template.Template
	pollInterval time.Duration
	handles      HandleStore
	log          *slog.Logger
}

// templateData is the render context for variable templates.
type templateData struct {
	Repo     string
	Platform string
	Reason   string
}

// New builds a gitlabPipeline Executor; variable templates are parsed and
// validated here so bad templates fail at startup. handles may be nil to
// disable run persistence (no re-adoption after restarts).
func New(cfg config.Executor, client *gogitlab.Client, handles HandleStore, log *slog.Logger) (*Executor, error) {
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
		handles:      handles,
		log:          log.With("executor", cfg.Name),
	}, nil
}

// Name returns the executor's configured name.
func (e *Executor) Name() string { return e.name }

// Run triggers the pipeline with rendered variables and polls it until a
// terminal status or ctx cancellation.
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

	if e.handles != nil {
		data, merr := json.Marshal(runHandle{
			Executor:   e.name,
			Platform:   spec.Repo.Platform,
			Repo:       spec.Repo.FullName,
			Project:    e.project,
			PipelineID: pipeline.ID,
		})
		if merr == nil {
			e.handles.SaveRunHandle(spec.Repo.Key(), string(data))
			defer e.handles.DeleteRunHandle(spec.Repo.Key())
		}
	}

	return e.pollPipeline(ctx, pipeline.ID, log)
}

// AdoptRunning resumes polling pipelines whose handles were persisted by a
// previous instance. Handles of other executors are left alone; corrupt
// ones are deleted.
func (e *Executor) AdoptRunning(_ context.Context) ([]executor.AdoptedRun, error) {
	if e.handles == nil {
		return nil, nil
	}
	var adopted []executor.AdoptedRun
	for key, data := range e.handles.LoadRunHandles() {
		var h runHandle
		if err := json.Unmarshal([]byte(data), &h); err != nil {
			e.log.Warn("deleting corrupt run handle", "repo", key, "error", err)
			e.handles.DeleteRunHandle(key)
			continue
		}
		if h.Executor != e.name {
			continue
		}
		repo := platform.Repo{Platform: h.Platform, FullName: h.Repo}
		pipelineID := h.PipelineID
		handleKey := key
		adopted = append(adopted, executor.AdoptedRun{
			Repo: repo,
			Wait: func(ctx context.Context) error {
				defer e.handles.DeleteRunHandle(handleKey)
				log := e.log.With("repo", repo.Key(), "pipeline", pipelineID, "adopted", true)
				log.Info("resuming pipeline poll")
				return e.pollPipeline(ctx, pipelineID, log)
			},
		})
	}
	return adopted, nil
}

func (e *Executor) pollPipeline(ctx context.Context, pipelineID int64, log *slog.Logger) error {
	ticker := time.NewTicker(e.pollInterval)
	defer ticker.Stop()
	pollErrors := 0
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
		}

		p, _, err := e.client.Pipelines.GetPipeline(e.project, pipelineID, gogitlab.WithContext(ctx))
		if err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			pollErrors++
			if pollErrors >= maxConsecutivePollErrors {
				return fmt.Errorf("poll pipeline %d: %w", pipelineID, err)
			}
			log.Warn("pipeline poll failed, retrying", "error", err, "attempt", pollErrors)
			continue
		}
		pollErrors = 0

		switch p.Status {
		case "success":
			return nil
		case "failed", "canceled", "skipped":
			return fmt.Errorf("pipeline %d finished with status %q", pipelineID, p.Status)
		default:
			// created|waiting_for_resource|preparing|pending|running|manual|scheduled: keep polling
		}
	}
}
