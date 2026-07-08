// Package history keeps a bounded in-memory record of finished runs for
// the read-only /api/v1/runs endpoint.
package history

import (
	"sync"
	"time"
)

// Entry describes one finished renovate run.
type Entry struct {
	Repo     string    `json:"repo"`
	Reason   string    `json:"reason"`
	Executor string    `json:"executor"`
	Result   string    `json:"result"`          // success|failure|timeout
	Error    string    `json:"error,omitempty"` // truncated
	Start    time.Time `json:"start"`
	Duration string    `json:"duration"`
}

// History is a fixed-size ring buffer of run entries, safe for concurrent
// use. When full, the oldest entry is overwritten.
type History struct {
	mu      sync.Mutex
	entries []Entry
	idx     int // next write position
	filled  bool
}

// New returns a History holding up to size entries (100 if size <= 0).
func New(size int) *History {
	if size <= 0 {
		size = 100
	}
	return &History{entries: make([]Entry, size)}
}

// Record appends an entry, evicting the oldest when full.
func (h *History) Record(e Entry) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.entries[h.idx] = e
	h.idx++
	if h.idx == len(h.entries) {
		h.idx = 0
		h.filled = true
	}
}

// Entries returns a copy of all recorded entries, newest first.
func (h *History) Entries() []Entry {
	h.mu.Lock()
	defer h.mu.Unlock()
	n := h.idx
	if h.filled {
		n = len(h.entries)
	}
	out := make([]Entry, 0, n)
	for i := 1; i <= n; i++ {
		out = append(out, h.entries[(h.idx-i+len(h.entries))%len(h.entries)])
	}
	return out
}
