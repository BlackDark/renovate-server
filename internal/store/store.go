// Package store tracks per-repo run state behind an interface so the
// in-memory default can later be swapped for a shared backend (Redis).
package store

import "time"

// State of a tracked repo. Idle repos are not tracked.
type State string

// Repo states; a repo without an entry is idle.
const (
	StateQueued  State = "queued"
	StateRunning State = "running"
)

// QueueResult tells the caller what a Queue call did.
type QueueResult int

const (
	// Queued means the repo was idle and the caller must schedule a run.
	Queued QueueResult = iota
	// Coalesced means the repo was already queued; the event merged into
	// the pending run.
	Coalesced
	// Deferred means the repo is running; a rerun was flagged for after
	// completion.
	Deferred
)

// RepoStatus is a point-in-time view of one repo, for the status API.
type RepoStatus struct {
	State        State     `json:"state"`
	Reason       string    `json:"reason"`
	Since        time.Time `json:"since"`
	PendingRerun bool      `json:"pendingRerun"`
}

// Store tracks per-repo run state. Implementations must be safe for
// concurrent use. The memory implementation is the default; the interface
// exists so a Redis-backed implementation can replace it later.
type Store interface {
	Queue(key, reason string) QueueResult
	StartRun(key string)
	// FinishRun releases the repo and reports whether a rerun was deferred
	// while it ran. The rerun flag is consumed.
	FinishRun(key string) (rerun bool)
	// Adopt marks a repo as running without going through Queue, used when
	// re-adopting in-flight runs after a restart.
	Adopt(key, reason string)
	Snapshot() map[string]RepoStatus

	// SaveRunHandle persists executor-specific state (e.g. a pipeline id)
	// for an active run so it can be re-adopted after a restart. One handle
	// per repo key; saving overwrites.
	SaveRunHandle(key, data string)
	// LoadRunHandles returns all persisted handles (repo key -> data).
	LoadRunHandles() map[string]string
	// DeleteRunHandle removes a run's handle; missing keys are a no-op.
	DeleteRunHandle(key string)
}
