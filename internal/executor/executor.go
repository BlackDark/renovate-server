// Package executor defines how renovate runs are started and awaited;
// implementations live in the subpackages gitlabci, kubernetes and docker.
package executor

import (
	"context"

	"github.com/BlackDark/renovate-server/internal/platform"
)

// RunSpec describes a single renovate run.
type RunSpec struct {
	Repo   platform.Repo
	Reason platform.Reason
}

// Executor starts renovate runs.
type Executor interface {
	Name() string
	// Run executes renovate for spec and blocks until the run finishes.
	// Implementations MUST honor ctx cancellation: the dispatcher enforces
	// the global run timeout through ctx.
	Run(ctx context.Context, spec RunSpec) error
}

// Adoptable is implemented by executors that can re-attach to runs already
// in flight after a server restart.
type Adoptable interface {
	AdoptRunning(ctx context.Context) ([]AdoptedRun, error)
}

// AdoptedRun is an in-flight run discovered at startup.
type AdoptedRun struct {
	Repo platform.Repo
	Wait func(ctx context.Context) error
}
