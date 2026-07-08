package docker

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/image"
	"github.com/docker/docker/api/types/network"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"

	"github.com/BlackDark/renovate-server/internal/config"
	"github.com/BlackDark/renovate-server/internal/executor"
	"github.com/BlackDark/renovate-server/internal/platform"
)

type fakeAPI struct {
	mu            sync.Mutex
	pulled        []string
	created       *container.Config
	hostCfg       *container.HostConfig
	started       bool
	removed       bool
	exitCode      int64
	waitDelay     time.Duration
	logsRequested bool
	logsTail      string
}

func (f *fakeAPI) ContainerLogs(_ context.Context, _ string, opts container.LogsOptions) (io.ReadCloser, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.logsRequested = true
	f.logsTail = opts.Tail
	// multiplexed stream: header{stream=1(stdout), len} + payload
	payload := []byte("ERROR: renovate failed\n")
	var buf bytes.Buffer
	buf.Write([]byte{1, 0, 0, 0, 0, 0, 0, byte(len(payload))})
	buf.Write(payload)
	return io.NopCloser(&buf), nil
}

func (f *fakeAPI) ImagePull(_ context.Context, ref string, _ image.PullOptions) (io.ReadCloser, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.pulled = append(f.pulled, ref)
	return io.NopCloser(strings.NewReader("{}")), nil
}

func (f *fakeAPI) ContainerCreate(_ context.Context, cfg *container.Config, host *container.HostConfig,
	_ *network.NetworkingConfig, _ *ocispec.Platform, _ string) (container.CreateResponse, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.created = cfg
	f.hostCfg = host
	return container.CreateResponse{ID: "cid-1"}, nil
}

func (f *fakeAPI) ContainerStart(_ context.Context, _ string, _ container.StartOptions) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.started = true
	return nil
}

func (f *fakeAPI) ContainerWait(ctx context.Context, _ string, _ container.WaitCondition) (<-chan container.WaitResponse, <-chan error) {
	respCh := make(chan container.WaitResponse, 1)
	errCh := make(chan error, 1)
	go func() {
		select {
		case <-time.After(f.waitDelay):
			f.mu.Lock()
			code := f.exitCode
			f.mu.Unlock()
			respCh <- container.WaitResponse{StatusCode: code}
		case <-ctx.Done():
			errCh <- ctx.Err()
		}
	}()
	return respCh, errCh
}

func (f *fakeAPI) ContainerRemove(_ context.Context, _ string, _ container.RemoveOptions) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.removed = true
	return nil
}

func (f *fakeAPI) wasRemoved() bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.removed
}

func testExecutor(api API, pull bool) *Executor {
	return New(config.Executor{
		Name:        "docker",
		Type:        config.ExecutorDocker,
		Image:       "renovate/renovate:41",
		CacheVolume: "renovate-cache",
		Pull:        pull,
		Env:         map[string]string{"LOG_LEVEL": "debug"},
	}, api, slog.New(slog.DiscardHandler))
}

func spec() executor.RunSpec {
	return executor.RunSpec{
		Repo:   platform.Repo{Platform: "gl", FullName: "top-group/app"},
		Reason: platform.ReasonPush,
	}
}

func TestRunSuccess(t *testing.T) {
	api := &fakeAPI{}
	e := testExecutor(api, false)
	if err := e.Run(t.Context(), spec()); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(api.pulled) != 0 {
		t.Errorf("pull not configured but pulled %v", api.pulled)
	}
	if api.created.Image != "renovate/renovate:41" {
		t.Errorf("image = %q", api.created.Image)
	}
	wantEnv := []string{"RENOVATE_REPOSITORIES=top-group/app", "LOG_LEVEL=debug"}
	for _, w := range wantEnv {
		found := false
		for _, got := range api.created.Env {
			if got == w {
				found = true
			}
		}
		if !found {
			t.Errorf("env missing %q in %v", w, api.created.Env)
		}
	}
	if len(api.hostCfg.Binds) != 1 || api.hostCfg.Binds[0] != "renovate-cache:/tmp/renovate/cache" {
		t.Errorf("binds = %v", api.hostCfg.Binds)
	}
	if !api.started || !api.wasRemoved() {
		t.Errorf("started=%v removed=%v, want both true", api.started, api.removed)
	}
	if api.logsRequested {
		t.Error("logs must not be fetched for successful runs")
	}
}

func TestRunPullsWhenConfigured(t *testing.T) {
	api := &fakeAPI{}
	e := testExecutor(api, true)
	if err := e.Run(t.Context(), spec()); err != nil {
		t.Fatal(err)
	}
	if len(api.pulled) != 1 || api.pulled[0] != "renovate/renovate:41" {
		t.Errorf("pulled = %v", api.pulled)
	}
}

func TestRunNonZeroExit(t *testing.T) {
	api := &fakeAPI{exitCode: 2}
	e := testExecutor(api, false)
	err := e.Run(t.Context(), spec())
	if err == nil || !strings.Contains(err.Error(), "exit code 2") {
		t.Fatalf("want exit code error, got %v", err)
	}
	if !api.wasRemoved() {
		t.Error("container not removed after failure")
	}
	if !api.logsRequested {
		t.Error("logs must be fetched for failed runs")
	}
	if api.logsTail != "50" {
		t.Errorf("logs tail = %q, want 50", api.logsTail)
	}
}

func TestRunContextCancelled(t *testing.T) {
	api := &fakeAPI{waitDelay: time.Minute}
	e := testExecutor(api, false)
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Millisecond)
	defer cancel()
	err := e.Run(ctx, spec())
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("want DeadlineExceeded, got %v", err)
	}
	if !api.wasRemoved() {
		t.Error("container not removed after cancellation")
	}
}
