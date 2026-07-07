package dispatch

import (
	"context"
	"testing"

	"github.com/BlackDark/renovate-server/internal/config"
	"github.com/BlackDark/renovate-server/internal/executor"
)

type fakeRouteExecutor struct{ name string }

func (f *fakeRouteExecutor) Name() string                                { return f.name }
func (f *fakeRouteExecutor) Run(context.Context, executor.RunSpec) error { return nil }

func TestRouterFirstMatchWins(t *testing.T) {
	ci := &fakeRouteExecutor{name: "ci"}
	k8s := &fakeRouteExecutor{name: "k8s"}
	r, err := NewRouter([]config.Rule{
		{Match: "top/legacy/**", Disabled: true},
		{Match: "top/platform/**", Executor: "k8s"},
		{Match: "**", Executor: "ci"},
	}, map[string]executor.Executor{"ci": ci, "k8s": k8s})
	if err != nil {
		t.Fatal(err)
	}

	cases := []struct {
		repo     string
		disabled bool
		executor string
	}{
		{"top/legacy/old-app", true, ""},
		{"top/legacy/sub/deep", true, ""},
		{"top/platform/api", false, "k8s"},
		{"top/other/app", false, "ci"},
		{"anything", false, "ci"},
	}
	for _, tc := range cases {
		got := r.Route(tc.repo)
		if got.Disabled != tc.disabled {
			t.Errorf("Route(%q).Disabled = %v, want %v", tc.repo, got.Disabled, tc.disabled)
		}
		if !tc.disabled && got.Executor.Name() != tc.executor {
			t.Errorf("Route(%q).Executor = %q, want %q", tc.repo, got.Executor.Name(), tc.executor)
		}
	}
}

func TestRouterUnknownExecutor(t *testing.T) {
	_, err := NewRouter([]config.Rule{{Match: "**", Executor: "ghost"}}, nil)
	if err == nil {
		t.Fatal("want error for unknown executor")
	}
}
