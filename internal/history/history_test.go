package history

import (
	"fmt"
	"strconv"
	"sync"
	"testing"
)

func TestRingBufferEvictsOldest(t *testing.T) {
	h := New(3)
	for i := range 5 {
		h.Record(Entry{Repo: fmt.Sprintf("r%d", i)})
	}
	got := h.Entries()
	if len(got) != 3 {
		t.Fatalf("len = %d, want 3", len(got))
	}
	if got[0].Repo != "r4" || got[2].Repo != "r2" {
		t.Fatalf("order wrong: %+v", got)
	}
}

func TestPartiallyFilled(t *testing.T) {
	h := New(10)
	h.Record(Entry{Repo: "a"})
	h.Record(Entry{Repo: "b"})
	got := h.Entries()
	if len(got) != 2 || got[0].Repo != "b" || got[1].Repo != "a" {
		t.Fatalf("entries = %+v", got)
	}
}

func TestZeroSizeDefaults(t *testing.T) {
	h := New(0)
	h.Record(Entry{Repo: "a"})
	if len(h.Entries()) != 1 {
		t.Fatal("default-sized buffer must accept entries")
	}
}

func TestConcurrentRecord(t *testing.T) {
	h := New(8)
	var wg sync.WaitGroup
	for i := range 32 {
		wg.Add(1)
		go func() { defer wg.Done(); h.Record(Entry{Repo: strconv.Itoa(i)}); h.Entries() }()
	}
	wg.Wait()
	if len(h.Entries()) != 8 {
		t.Fatal("buffer size violated")
	}
}
