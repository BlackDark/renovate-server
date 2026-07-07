// Package docker runs renovate as containers against a local Docker daemon.
package docker

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"sort"
	"time"

	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/image"
	"github.com/docker/docker/api/types/network"
	"github.com/docker/docker/client"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"

	"github.com/BlackDark/renovate-server/internal/config"
	"github.com/BlackDark/renovate-server/internal/executor"
)

const (
	cacheMountPath = "/tmp/renovate/cache"
	removeTimeout  = 30 * time.Second
)

// API is the subset of the Docker SDK the executor needs; *client.Client
// satisfies it, tests use a fake.
type API interface {
	ImagePull(ctx context.Context, ref string, opts image.PullOptions) (io.ReadCloser, error)
	ContainerCreate(ctx context.Context, cfg *container.Config, host *container.HostConfig,
		netCfg *network.NetworkingConfig, platform *ocispec.Platform, name string) (container.CreateResponse, error)
	ContainerStart(ctx context.Context, id string, opts container.StartOptions) error
	ContainerWait(ctx context.Context, id string, cond container.WaitCondition) (<-chan container.WaitResponse, <-chan error)
	ContainerRemove(ctx context.Context, id string, opts container.RemoveOptions) error
}

// NewAPIFromEnv creates a Docker client from DOCKER_HOST etc.
func NewAPIFromEnv() (API, error) {
	return client.NewClientWithOpts(client.FromEnv, client.WithAPIVersionNegotiation())
}

type Executor struct {
	name        string
	api         API
	image       string
	cacheVolume string
	pull        bool
	env         map[string]string
	log         *slog.Logger
}

func New(cfg config.Executor, api API, log *slog.Logger) *Executor {
	return &Executor{
		name:        cfg.Name,
		api:         api,
		image:       cfg.Image,
		cacheVolume: cfg.CacheVolume,
		pull:        cfg.Pull,
		env:         cfg.Env,
		log:         log.With("executor", cfg.Name),
	}
}

func (e *Executor) Name() string { return e.name }

func (e *Executor) Run(ctx context.Context, spec executor.RunSpec) error {
	if e.pull {
		rc, err := e.api.ImagePull(ctx, e.image, image.PullOptions{})
		if err != nil {
			return fmt.Errorf("pull image %q: %w", e.image, err)
		}
		_, _ = io.Copy(io.Discard, rc)
		_ = rc.Close()
	}

	env := []string{"RENOVATE_REPOSITORIES=" + spec.Repo.FullName}
	keys := make([]string, 0, len(e.env))
	for k := range e.env {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		env = append(env, k+"="+e.env[k])
	}

	hostCfg := &container.HostConfig{}
	if e.cacheVolume != "" {
		hostCfg.Binds = []string{e.cacheVolume + ":" + cacheMountPath}
	}

	created, err := e.api.ContainerCreate(ctx, &container.Config{
		Image: e.image,
		Env:   env,
	}, hostCfg, nil, nil, "")
	if err != nil {
		return fmt.Errorf("create container: %w", err)
	}
	log := e.log.With("repo", spec.Repo.Key(), "container", created.ID)

	// Removal must succeed even when ctx is already cancelled.
	defer func() {
		removeCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), removeTimeout)
		defer cancel()
		if err := e.api.ContainerRemove(removeCtx, created.ID, container.RemoveOptions{Force: true}); err != nil {
			log.Warn("container remove failed", "error", err)
		}
	}()

	if err := e.api.ContainerStart(ctx, created.ID, container.StartOptions{}); err != nil {
		return fmt.Errorf("start container: %w", err)
	}
	log.Info("container started")

	respCh, errCh := e.api.ContainerWait(ctx, created.ID, container.WaitConditionNotRunning)
	select {
	case resp := <-respCh:
		if resp.StatusCode != 0 {
			return fmt.Errorf("renovate container finished with exit code %d", resp.StatusCode)
		}
		return nil
	case err := <-errCh:
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return fmt.Errorf("wait for container: %w", err)
	case <-ctx.Done():
		return ctx.Err()
	}
}
