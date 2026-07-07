// Package dispatch owns the run lifecycle: routing repos to executors and
// serializing runs per repo with debounce, coalescing and timeouts.
package dispatch

import (
	"context"
	"errors"
	"log/slog"
	"sync"
	"time"

	"github.com/BlackDark/renovate-server/internal/executor"
	"github.com/BlackDark/renovate-server/internal/platform"
	"github.com/BlackDark/renovate-server/internal/store"
)

// Metrics receives run lifecycle notifications. Implemented by the metrics
// package; nil-safe via the noop default.
type Metrics interface {
	RunStarted(executorName string)
	RunFinished(executorName, result string, seconds float64)
}

type noopMetrics struct{}

func (noopMetrics) RunStarted(string)                   {}
func (noopMetrics) RunFinished(string, string, float64) {}

// Options configures a Dispatcher.
type Options struct {
	Debounce      time.Duration
	RunTimeout    time.Duration
	MaxConcurrent int
	Log           *slog.Logger
	Metrics       Metrics
}

// Dispatcher owns the per-repo run lifecycle: debounce, mutual exclusion,
// rerun coalescing, global concurrency and the run timeout.
type Dispatcher struct {
	store   store.Store
	router  *Router
	opts    Options
	sem     chan struct{}
	wg      sync.WaitGroup
	baseCtx context.Context
	cancel  context.CancelFunc
}

// NewDispatcher wires a Dispatcher; opts.Log and opts.Metrics may be nil.
func NewDispatcher(st store.Store, router *Router, opts Options) *Dispatcher {
	if opts.Log == nil {
		opts.Log = slog.Default()
	}
	if opts.Metrics == nil {
		opts.Metrics = noopMetrics{}
	}
	ctx, cancel := context.WithCancel(context.Background())
	return &Dispatcher{
		store:   st,
		router:  router,
		opts:    opts,
		sem:     make(chan struct{}, opts.MaxConcurrent),
		baseCtx: ctx,
		cancel:  cancel,
	}
}

// Enqueue requests a run for the event's repo. Duplicate requests coalesce:
// one pending run while queued, one deferred rerun while running.
func (d *Dispatcher) Enqueue(ev platform.Event) {
	log := d.opts.Log.With("repo", ev.Repo.Key(), "reason", string(ev.Reason))
	route := d.router.Route(ev.Repo.FullName)
	if route.Disabled {
		log.Debug("repo disabled by rules, ignoring")
		return
	}

	switch d.store.Queue(ev.Repo.Key(), string(ev.Reason)) {
	case store.Queued:
		log.Info("run queued")
		d.wg.Add(1)
		go d.run(ev, route.Executor)
	case store.Coalesced:
		log.Debug("event coalesced into queued run")
	case store.Deferred:
		log.Info("run in flight, rerun scheduled")
	}
}

// Adopt registers an in-flight run discovered at startup. The repo is
// locked until wait returns; a deferred rerun fires afterwards if events
// arrived meanwhile.
func (d *Dispatcher) Adopt(run executor.AdoptedRun, executorName string) {
	key := run.Repo.Key()
	d.store.Adopt(key, string(platform.ReasonRerun))
	d.opts.Log.Info("adopted in-flight run", "repo", key, "executor", executorName)
	d.wg.Add(1)
	go func() {
		defer d.wg.Done()
		ctx, cancel := context.WithTimeout(d.baseCtx, d.opts.RunTimeout)
		defer cancel()
		err := run.Wait(ctx)
		d.finish(run.Repo, executorName, time.Now(), err)
	}()
}

func (d *Dispatcher) run(ev platform.Event, exec executor.Executor) {
	defer d.wg.Done()

	select {
	case <-time.After(d.opts.Debounce):
	case <-d.baseCtx.Done():
		d.store.FinishRun(ev.Repo.Key())
		return
	}

	select {
	case d.sem <- struct{}{}:
	case <-d.baseCtx.Done():
		d.store.FinishRun(ev.Repo.Key())
		return
	}
	defer func() { <-d.sem }()

	d.store.StartRun(ev.Repo.Key())
	d.opts.Metrics.RunStarted(exec.Name())
	start := time.Now()

	ctx, cancel := context.WithTimeout(d.baseCtx, d.opts.RunTimeout)
	defer cancel()
	err := exec.Run(ctx, executor.RunSpec{Repo: ev.Repo, Reason: ev.Reason})
	d.finish(ev.Repo, exec.Name(), start, err)
}

func (d *Dispatcher) finish(repo platform.Repo, executorName string, start time.Time, err error) {
	log := d.opts.Log.With("repo", repo.Key(), "executor", executorName)
	result := "success"
	switch {
	case err == nil:
		log.Info("run finished", "duration", time.Since(start))
	case errors.Is(err, context.DeadlineExceeded):
		result = "timeout"
		log.Error("run timed out, lock released", "timeout", d.opts.RunTimeout)
	default:
		result = "failure"
		log.Error("run failed", "error", err)
	}
	d.opts.Metrics.RunFinished(executorName, result, time.Since(start).Seconds())

	if rerun := d.store.FinishRun(repo.Key()); rerun {
		log.Info("deferred rerun triggered")
		d.Enqueue(platform.Event{Repo: repo, Reason: platform.ReasonRerun})
	}
}

// Shutdown stops accepting timed work and waits for in-flight runs.
func (d *Dispatcher) Shutdown(ctx context.Context) error {
	done := make(chan struct{})
	go func() {
		d.wg.Wait()
		close(done)
	}()
	select {
	case <-done:
		d.cancel()
		return nil
	case <-ctx.Done():
		d.cancel()
		return ctx.Err()
	}
}
