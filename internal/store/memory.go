package store

import (
	"sync"
	"time"
)

type repoState struct {
	state        State
	reason       string
	since        time.Time
	pendingRerun bool
}

type memory struct {
	mu    sync.Mutex
	repos map[string]*repoState
}

// NewMemory returns an in-memory Store. State is lost on restart; the run
// timeout and cron schedules heal any resulting stuck state.
func NewMemory() Store {
	return &memory{repos: make(map[string]*repoState)}
}

func (m *memory) Queue(key, reason string) QueueResult {
	m.mu.Lock()
	defer m.mu.Unlock()
	rs, ok := m.repos[key]
	if !ok {
		m.repos[key] = &repoState{state: StateQueued, reason: reason, since: time.Now()}
		return Queued
	}
	if rs.state == StateRunning {
		rs.pendingRerun = true
		return Deferred
	}
	return Coalesced
}

func (m *memory) StartRun(key string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if rs, ok := m.repos[key]; ok {
		rs.state = StateRunning
		rs.since = time.Now()
	}
}

func (m *memory) FinishRun(key string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	rs, ok := m.repos[key]
	if !ok {
		return false
	}
	rerun := rs.pendingRerun
	delete(m.repos, key)
	return rerun
}

func (m *memory) Adopt(key, reason string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.repos[key]; !ok {
		m.repos[key] = &repoState{state: StateRunning, reason: reason, since: time.Now()}
	}
}

func (m *memory) Snapshot() map[string]RepoStatus {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make(map[string]RepoStatus, len(m.repos))
	for k, rs := range m.repos {
		out[k] = RepoStatus{State: rs.state, Reason: rs.reason, Since: rs.since, PendingRerun: rs.pendingRerun}
	}
	return out
}
