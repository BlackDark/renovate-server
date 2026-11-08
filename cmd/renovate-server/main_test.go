package main

import (
	"testing"

	"github.com/BlackDark/renovate-server/internal/config"
)

func TestReloadRefused(t *testing.T) {
	base := &config.Config{}
	base.Server.Listen = ":8080"

	same := *base
	if reloadRefused(base, &same) {
		t.Fatal("identical config refused")
	}

	listen := *base
	listen.Server.Listen = ":9090"
	if !reloadRefused(base, &listen) {
		t.Fatal("listen change allowed")
	}

	platforms := *base
	platforms.Platforms = []config.Platform{{Name: "gl"}}
	if !reloadRefused(base, &platforms) {
		t.Fatal("platform change allowed")
	}

	executors := *base
	executors.Executors = []config.Executor{{Name: "shadow"}}
	if !reloadRefused(base, &executors) {
		t.Fatal("executor change allowed")
	}

	rules := *base
	rules.Rules = []config.Rule{{Match: "**"}}
	if reloadRefused(base, &rules) {
		t.Fatal("rules change refused")
	}
}
