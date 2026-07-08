package dispatch

import (
	"context"
	"log/slog"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/BlackDark/renovate-server/internal/config"
	"github.com/BlackDark/renovate-server/internal/executor"
	"github.com/BlackDark/renovate-server/internal/history"
	"github.com/BlackDark/renovate-server/internal/platform"
	"github.com/BlackDark/renovate-server/internal/store"
)

// blockingExecutor records runs and blocks each until released.
type blockingExecutor struct {
	mu      sync.Mutex
	runs    []executor.RunSpec
	release chan struct{}
	active  atomic.Int32
	maxSeen atomic.Int32
}

func newBlockingExecutor() *blockingExecutor {
	return &blockingExecutor{release: make(chan struct{})}
}

func (b *blockingExecutor) Name() string { return "fake" }

func (b *blockingExecutor) Run(ctx context.Context, spec executor.RunSpec) error {
	b.mu.Lock()
	b.runs = append(b.runs, spec)
	b.mu.Unlock()
	n := b.active.Add(1)
	for {
		prev := b.maxSeen.Load()
		if n <= prev || b.maxSeen.CompareAndSwap(prev, n) {
			break
		}
	}
	defer b.active.Add(-1)
	select {
	case <-b.release:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (b *blockingExecutor) runCount() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return len(b.runs)
}

func testDispatcher(t *testing.T, exec executor.Executor, opts Options) *Dispatcher {
	t.Helper()
	router, err := NewRouter(
		[]config.Rule{{Match: "**", Executor: "fake"}},
		map[string]executor.Executor{"fake": exec},
	)
	if err != nil {
		t.Fatal(err)
	}
	if opts.Log == nil {
		opts.Log = slog.New(slog.DiscardHandler)
	}
	if opts.Debounce == 0 {
		opts.Debounce = time.Millisecond
	}
	if opts.RunTimeout == 0 {
		opts.RunTimeout = time.Minute
	}
	if opts.MaxConcurrent == 0 {
		opts.MaxConcurrent = 4
	}
	return NewDispatcher(store.NewMemory(), router, opts)
}

func event(name string) platform.Event {
	return platform.Event{
		Repo:   platform.Repo{Platform: "gl", FullName: name},
		Reason: platform.ReasonPush,
	}
}

func waitFor(t *testing.T, cond func() bool, msg string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatal("timeout waiting for: " + msg)
}

func TestDebounceCoalescesEvents(t *testing.T) {
	exec := newBlockingExecutor()
	d := testDispatcher(t, exec, Options{Debounce: 50 * time.Millisecond})
	for range 10 {
		d.Enqueue(event("g/a"))
	}
	waitFor(t, func() bool { return exec.runCount() == 1 }, "one run")
	close(exec.release)
	shutdown(t, d)
	if got := exec.runCount(); got != 1 {
		t.Fatalf("runs = %d, want 1", got)
	}
}

func TestEventDuringRunTriggersExactlyOneRerun(t *testing.T) {
	exec := newBlockingExecutor()
	d := testDispatcher(t, exec, Options{})
	d.Enqueue(event("g/a"))
	waitFor(t, func() bool { return exec.active.Load() == 1 }, "run started")

	// events while running: all coalesce into one rerun
	d.Enqueue(event("g/a"))
	d.Enqueue(event("g/a"))
	d.Enqueue(event("g/a"))

	close(exec.release) // releases current and all future runs
	waitFor(t, func() bool { return exec.runCount() == 2 }, "rerun happened")
	shutdown(t, d)
	if got := exec.runCount(); got != 2 {
		t.Fatalf("runs = %d, want 2 (original + one rerun)", got)
	}
}

func TestGlobalConcurrencyLimit(t *testing.T) {
	exec := newBlockingExecutor()
	d := testDispatcher(t, exec, Options{MaxConcurrent: 2})
	for _, r := range []string{"g/a", "g/b", "g/c", "g/d", "g/e"} {
		d.Enqueue(event(r))
	}
	waitFor(t, func() bool { return exec.active.Load() == 2 }, "2 active")
	time.Sleep(20 * time.Millisecond) // give extras a chance to (wrongly) start
	if got := exec.maxSeen.Load(); got != 2 {
		t.Fatalf("max concurrent = %d, want 2", got)
	}
	close(exec.release)
	shutdown(t, d)
	if got := exec.runCount(); got != 5 {
		t.Fatalf("runs = %d, want 5", got)
	}
}

func TestRunTimeoutReleasesLock(t *testing.T) {
	exec := newBlockingExecutor() // never released -> only ctx ends runs
	d := testDispatcher(t, exec, Options{RunTimeout: 30 * time.Millisecond})
	d.Enqueue(event("g/a"))
	waitFor(t, func() bool { return exec.runCount() == 1 && exec.active.Load() == 0 }, "timed-out run finished")
	// lock released: a new event can run again
	d.Enqueue(event("g/a"))
	waitFor(t, func() bool { return exec.runCount() == 2 }, "second run after timeout")
	shutdown(t, d)
}

func TestDisabledRepoNeverRuns(t *testing.T) {
	exec := newBlockingExecutor()
	router, err := NewRouter([]config.Rule{
		{Match: "g/off/**", Disabled: true},
		{Match: "**", Executor: "fake"},
	}, map[string]executor.Executor{"fake": exec})
	if err != nil {
		t.Fatal(err)
	}
	d := NewDispatcher(store.NewMemory(), router, Options{
		Debounce: time.Millisecond, RunTimeout: time.Minute, MaxConcurrent: 1,
		Log: slog.New(slog.DiscardHandler),
	})
	d.Enqueue(event("g/off/app"))
	time.Sleep(30 * time.Millisecond)
	if exec.runCount() != 0 {
		t.Fatalf("disabled repo ran %d times", exec.runCount())
	}
	shutdown(t, d)
}

func TestAdoptedRunBlocksNewRunsUntilDone(t *testing.T) {
	exec := newBlockingExecutor()
	d := testDispatcher(t, exec, Options{})
	adoptedDone := make(chan struct{})
	d.Adopt(executor.AdoptedRun{
		Repo: platform.Repo{Platform: "gl", FullName: "g/a"},
		Wait: func(ctx context.Context) error {
			select {
			case <-adoptedDone:
				return nil
			case <-ctx.Done():
				return ctx.Err()
			}
		},
	}, "fake")

	d.Enqueue(event("g/a")) // must defer, not start
	time.Sleep(20 * time.Millisecond)
	if exec.runCount() != 0 {
		t.Fatal("run started while adopted run in flight")
	}
	close(adoptedDone)
	close(exec.release)
	waitFor(t, func() bool { return exec.runCount() == 1 }, "deferred run after adoption finished")
	shutdown(t, d)
}

type fakeRecorder struct {
	mu      sync.Mutex
	entries []history.Entry
}

func (f *fakeRecorder) Record(e history.Entry) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.entries = append(f.entries, e)
}

func (f *fakeRecorder) all() []history.Entry {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]history.Entry(nil), f.entries...)
}

func TestHistoryRecordsRuns(t *testing.T) {
	exec := newBlockingExecutor()
	close(exec.release)
	rec := &fakeRecorder{}
	d := testDispatcher(t, exec, Options{History: rec})
	d.Enqueue(event("g/a"))
	waitFor(t, func() bool { return len(rec.all()) == 1 }, "entry recorded")
	shutdown(t, d)

	e := rec.all()[0]
	if e.Repo != "gl:g/a" || e.Reason != "push" || e.Executor != "fake" || e.Result != "success" {
		t.Fatalf("entry = %+v", e)
	}
	if e.Start.IsZero() || e.Duration == "" {
		t.Fatalf("timing missing: %+v", e)
	}
	if e.Error != "" {
		t.Fatalf("no error expected: %+v", e)
	}
}

func TestHistoryRecordsTimeout(t *testing.T) {
	exec := newBlockingExecutor() // never released
	rec := &fakeRecorder{}
	d := testDispatcher(t, exec, Options{History: rec, RunTimeout: 20 * time.Millisecond})
	d.Enqueue(event("g/a"))
	waitFor(t, func() bool { return len(rec.all()) == 1 }, "entry recorded")
	shutdown(t, d)

	e := rec.all()[0]
	if e.Result != "timeout" || e.Error == "" {
		t.Fatalf("entry = %+v", e)
	}
}

func shutdown(t *testing.T, d *Dispatcher) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := d.Shutdown(ctx); err != nil {
		t.Fatalf("shutdown: %v", err)
	}
}
