// Command renovate-server coordinates renovate runs across GitLab/GitHub
// repositories, triggered by webhooks and cron schedules.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"reflect"
	"strings"
	"syscall"
	"time"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/BlackDark/renovate-server/internal/config"
	"github.com/BlackDark/renovate-server/internal/dispatch"
	"github.com/BlackDark/renovate-server/internal/executor"
	dockerexec "github.com/BlackDark/renovate-server/internal/executor/docker"
	"github.com/BlackDark/renovate-server/internal/executor/gitlabci"
	kubeexec "github.com/BlackDark/renovate-server/internal/executor/kubernetes"
	"github.com/BlackDark/renovate-server/internal/history"
	"github.com/BlackDark/renovate-server/internal/metrics"
	"github.com/BlackDark/renovate-server/internal/platform"
	githubplatform "github.com/BlackDark/renovate-server/internal/platform/github"
	gitlabplatform "github.com/BlackDark/renovate-server/internal/platform/gitlab"
	"github.com/BlackDark/renovate-server/internal/schedule"
	"github.com/BlackDark/renovate-server/internal/server"
	"github.com/BlackDark/renovate-server/internal/store"
)

var version = "dev" // set via -ldflags "-X main.version=..."

func main() {
	configPath := flag.String("config", "/etc/renovate-server/config.yaml", "path to config file")
	showVersion := flag.Bool("version", false, "print version and exit")
	flag.Parse()

	if *showVersion {
		fmt.Println("renovate-server", version)
		return
	}

	if err := run(*configPath); err != nil {
		slog.Error("fatal", "error", err)
		os.Exit(1)
	}
}

func run(configPath string) error {
	cfg, err := config.Load(configPath)
	if err != nil {
		return err
	}

	log, levelVar, err := newLogger(cfg.Server.Log)
	if err != nil {
		return err
	}
	slog.SetDefault(log)
	log.Info("starting renovate-server", "version", version)

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	// Platforms.
	platforms := make([]platform.Platform, 0, len(cfg.Platforms))
	gitlabPlatforms := map[string]*gitlabplatform.GitLab{}
	for _, pc := range cfg.Platforms {
		switch pc.Type {
		case config.PlatformGitLab:
			p, err := gitlabplatform.New(pc, log)
			if err != nil {
				return fmt.Errorf("platform %q: %w", pc.Name, err)
			}
			platforms = append(platforms, p)
			gitlabPlatforms[pc.Name] = p
		case config.PlatformGitHub:
			p, err := githubplatform.New(pc, log)
			if err != nil {
				return fmt.Errorf("platform %q: %w", pc.Name, err)
			}
			platforms = append(platforms, p)
		}
	}

	// Executors.
	executors := map[string]executor.Executor{}
	for _, ec := range cfg.Executors {
		switch ec.Type {
		case config.ExecutorGitLabPipeline:
			gl, ok := gitlabPlatforms[ec.Platform]
			if !ok {
				return fmt.Errorf("executor %q: platform %q is not a configured gitlab platform", ec.Name, ec.Platform)
			}
			ex, err := gitlabci.New(ec, gl.Client(), log)
			if err != nil {
				return err
			}
			executors[ec.Name] = ex
		case config.ExecutorKubernetes:
			client, err := kubeexec.NewClientFromEnv()
			if err != nil {
				return fmt.Errorf("executor %q: %w", ec.Name, err)
			}
			executors[ec.Name] = kubeexec.New(ec, client, log)
		case config.ExecutorDocker:
			api, err := dockerexec.NewAPIFromEnv()
			if err != nil {
				return fmt.Errorf("executor %q: %w", ec.Name, err)
			}
			executors[ec.Name] = dockerexec.New(ec, api, log)
		}
	}

	// Core.
	var st store.Store
	if cfg.Server.Store.Type == "redis" {
		st, err = store.NewRedis(ctx, cfg.Server.Store.Redis)
		if err != nil {
			return fmt.Errorf("redis store: %w", err)
		}
	} else {
		st = store.NewMemory()
	}
	reg := prometheus.NewRegistry()
	m := metrics.New(reg, st)
	hist := history.New(cfg.Server.HistorySize)
	router, err := dispatch.NewRouter(cfg.Rules, executors)
	if err != nil {
		return err
	}
	disp := dispatch.NewDispatcher(st, router, dispatch.Options{
		Debounce:      cfg.Server.Debounce,
		RunTimeout:    cfg.Server.RunTimeout,
		MaxConcurrent: cfg.Server.MaxConcurrentRuns,
		Log:           log,
		Metrics:       m,
		History:       hist,
	})

	// Re-adopt in-flight runs (kubernetes executor).
	for name, ex := range executors {
		adoptable, ok := ex.(executor.Adoptable)
		if !ok {
			continue
		}
		runs, err := adoptable.AdoptRunning(ctx)
		if err != nil {
			log.Warn("re-adoption failed, relying on run timeout", "executor", name, "error", err)
			continue
		}
		for _, run := range runs {
			disp.Adopt(run, name)
		}
	}

	// Persistent store: re-enqueue repos that were still queued when the
	// previous instance stopped. Runs adopted above already re-locked their
	// repos; unadopted "running" markers expire via the store TTL.
	if cfg.Server.Store.Type == "redis" {
		requeued := 0
		for key, status := range st.Snapshot() {
			if status.State != store.StateQueued {
				continue
			}
			parts := strings.SplitN(key, ":", 2)
			if len(parts) != 2 {
				continue
			}
			st.FinishRun(key) // clear the stale entry so Enqueue starts fresh
			disp.Enqueue(platform.Event{
				Repo:   platform.Repo{Platform: parts[0], FullName: parts[1]},
				Reason: platform.ReasonRerun,
			})
			requeued++
		}
		log.Info("store recovery finished", "requeuedRepos", requeued)
	}

	// Cron schedules.
	sched := schedule.New(log)
	for _, p := range platforms {
		err := sched.AddPlatform(p.Schedule(), func() {
			log.Info("cron discovery started", "platform", p.Name())
			repos, err := p.DiscoverRepos(ctx)
			if err != nil {
				log.Error("cron discovery failed", "platform", p.Name(), "error", err)
				return
			}
			log.Info("cron discovery finished", "platform", p.Name(), "repos", len(repos))
			for _, repo := range repos {
				disp.Enqueue(platform.Event{Repo: repo, Reason: platform.ReasonCron})
			}
		})
		if err != nil {
			return fmt.Errorf("platform %q: %w", p.Name(), err)
		}
	}
	sched.Start()

	// SIGHUP reloads rules and log level. Platform/executor changes need a
	// restart: refusing them here avoids half-applied configurations.
	hup := make(chan os.Signal, 1)
	signal.Notify(hup, syscall.SIGHUP)
	go func() {
		for range hup {
			newCfg, err := config.Load(configPath)
			if err != nil {
				log.Error("config reload failed, keeping old config", "error", err)
				continue
			}
			if !reflect.DeepEqual(newCfg.Platforms, cfg.Platforms) || !reflect.DeepEqual(newCfg.Executors, cfg.Executors) {
				log.Error("platforms/executors changed: reload refused, restart required")
				continue
			}
			newRouter, err := dispatch.NewRouter(newCfg.Rules, executors)
			if err != nil {
				log.Error("config reload failed, keeping old rules", "error", err)
				continue
			}
			disp.SetRouter(newRouter)
			var lvl slog.Level
			if err := lvl.UnmarshalText([]byte(newCfg.Server.Log.Level)); err == nil {
				levelVar.Set(lvl)
			}
			cfg.Rules = newCfg.Rules
			log.Info("config reloaded", "rules", len(newCfg.Rules), "logLevel", newCfg.Server.Log.Level)
		}
	}()

	// HTTP server.
	srv := server.New(platforms, disp, st, hist, reg, m, log)
	httpServer := &http.Server{
		Addr:              cfg.Server.Listen,
		Handler:           srv.Handler(),
		ReadHeaderTimeout: 10 * time.Second,
	}
	serveErr := make(chan error, 1)
	go func() {
		log.Info("listening", "addr", cfg.Server.Listen)
		if err := httpServer.ListenAndServe(); !errors.Is(err, http.ErrServerClosed) {
			serveErr <- err
		}
	}()
	srv.SetReady(true)

	select {
	case err := <-serveErr:
		return err
	case <-ctx.Done():
	}

	log.Info("shutting down")
	srv.SetReady(false)
	sched.Stop()
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	_ = httpServer.Shutdown(shutdownCtx)
	if err := disp.Shutdown(shutdownCtx); err != nil {
		log.Warn("runs still in flight at shutdown", "error", err)
	}
	return nil
}

func newLogger(cfg config.Log) (*slog.Logger, *slog.LevelVar, error) {
	var level slog.Level
	if err := level.UnmarshalText([]byte(cfg.Level)); err != nil {
		return nil, nil, fmt.Errorf("invalid log level %q", cfg.Level)
	}
	levelVar := new(slog.LevelVar)
	levelVar.Set(level)
	opts := &slog.HandlerOptions{Level: levelVar}
	var handler slog.Handler
	switch cfg.Format {
	case "json":
		handler = slog.NewJSONHandler(os.Stderr, opts)
	case "text":
		handler = slog.NewTextHandler(os.Stderr, opts)
	default:
		return nil, nil, fmt.Errorf("invalid log format %q", cfg.Format)
	}
	return slog.New(handler), levelVar, nil
}
