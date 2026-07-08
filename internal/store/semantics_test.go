package store

import (
	"sync"
	"testing"
)

// testStoreSemantics verifies the Store contract; every implementation
// must pass it.
func testStoreSemantics(t *testing.T, s Store) {
	t.Helper()

	t.Run("queue transitions", func(t *testing.T) {
		if got := s.Queue("gl:g/p", "push"); got != Queued {
			t.Fatalf("first Queue = %v, want Queued", got)
		}
		if got := s.Queue("gl:g/p", "issue"); got != Coalesced {
			t.Fatalf("second Queue = %v, want Coalesced", got)
		}

		s.StartRun("gl:g/p")
		if got := s.Queue("gl:g/p", "merge_request"); got != Deferred {
			t.Fatalf("Queue while running = %v, want Deferred", got)
		}

		if rerun := s.FinishRun("gl:g/p"); !rerun {
			t.Fatal("FinishRun should report pending rerun")
		}
		// rerun flag consumed; repo idle again
		if got := s.Queue("gl:g/p", "push"); got != Queued {
			t.Fatalf("Queue after finish = %v, want Queued", got)
		}
		s.StartRun("gl:g/p")
		if rerun := s.FinishRun("gl:g/p"); rerun {
			t.Fatal("FinishRun without deferred event should not rerun")
		}
		if len(s.Snapshot()) != 0 {
			t.Fatalf("idle repos must be evicted, snapshot = %v", s.Snapshot())
		}
	})

	t.Run("snapshot", func(t *testing.T) {
		s.Queue("gl:a", "push")
		s.Queue("gl:b", "cron")
		s.StartRun("gl:b")
		s.Queue("gl:b", "issue")

		snap := s.Snapshot()
		if snap["gl:a"].State != StateQueued || snap["gl:a"].Reason != "push" {
			t.Errorf("gl:a = %+v", snap["gl:a"])
		}
		if snap["gl:b"].State != StateRunning || !snap["gl:b"].PendingRerun {
			t.Errorf("gl:b = %+v", snap["gl:b"])
		}
		if snap["gl:b"].Since.IsZero() {
			t.Error("Since must be set")
		}
		s.FinishRun("gl:a")
		s.FinishRun("gl:b")
	})

	t.Run("adopt", func(t *testing.T) {
		s.Adopt("gl:ad", "adopted")
		if snap := s.Snapshot(); snap["gl:ad"].State != StateRunning {
			t.Fatalf("adopted repo state = %+v", snap["gl:ad"])
		}
		if got := s.Queue("gl:ad", "push"); got != Deferred {
			t.Fatalf("Queue on adopted = %v, want Deferred", got)
		}
		s.FinishRun("gl:ad")
	})

	t.Run("concurrent access", func(t *testing.T) {
		var wg sync.WaitGroup
		for range 50 {
			wg.Add(1)
			go func() {
				defer wg.Done()
				if s.Queue("gl:x", "push") == Queued {
					s.StartRun("gl:x")
					s.FinishRun("gl:x")
				}
				s.Snapshot()
			}()
		}
		wg.Wait()
		s.FinishRun("gl:x")
	})
}
