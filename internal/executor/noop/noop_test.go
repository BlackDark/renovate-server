package noop

import (
	"log/slog"
	"testing"

	"github.com/BlackDark/renovate-server/internal/config"
	"github.com/BlackDark/renovate-server/internal/executor"
	"github.com/BlackDark/renovate-server/internal/platform"
)

func TestRunSucceedsWithoutSideEffects(t *testing.T) {
	e := New(config.Executor{Name: "shadow", Type: config.ExecutorNoop}, slog.New(slog.DiscardHandler))
	if e.Name() != "shadow" {
		t.Errorf("name = %q", e.Name())
	}
	err := e.Run(t.Context(), executor.RunSpec{
		Repo:   platform.Repo{Platform: "gl", FullName: "g/app"},
		Reason: platform.ReasonMergeRequest,
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
}
