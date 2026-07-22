package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func writeConfig(t *testing.T, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

const validConfig = `
server:
  listen: ":9090"
platforms:
  - name: gl
    type: gitlab
    baseURL: https://gitlab.example.com
    token: ${TEST_GL_TOKEN}
    webhook:
      path: /webhook/gitlab
      secret: ${TEST_GL_SECRET}
    events: [merge_request, issue]
    discovery:
      groups: [top-group]
      excludeArchived: true
    schedule:
      crontabs: ["0 3 * * *"]
      timezone: Europe/Berlin
executors:
  - name: ci
    type: gitlabPipeline
    platform: gl
    project: infra/renovate-runner
    triggerToken: tok
rules:
  - match: "top-group/legacy/**"
    disabled: true
  - match: "**"
    executor: ci
`

func TestLoadValidConfig(t *testing.T) {
	t.Setenv("TEST_GL_TOKEN", "glpat-abc")
	t.Setenv("TEST_GL_SECRET", "hooksecret")
	cfg, err := Load(writeConfig(t, validConfig))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Server.Listen != ":9090" {
		t.Errorf("listen = %q, want :9090", cfg.Server.Listen)
	}
	if cfg.Platforms[0].Token != "glpat-abc" {
		t.Errorf("token not expanded: %q", cfg.Platforms[0].Token)
	}
	if cfg.Platforms[0].Webhook.Secret != "hooksecret" {
		t.Errorf("secret not expanded: %q", cfg.Platforms[0].Webhook.Secret)
	}
}

func TestLoadDefaults(t *testing.T) {
	t.Setenv("TEST_GL_TOKEN", "x")
	t.Setenv("TEST_GL_SECRET", "y")
	cfg, err := Load(writeConfig(t, validConfig))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Server.Debounce != 10*time.Second {
		t.Errorf("debounce = %v, want 10s", cfg.Server.Debounce)
	}
	if cfg.Server.MaxConcurrentRuns != 4 {
		t.Errorf("maxConcurrentRuns = %d, want 4", cfg.Server.MaxConcurrentRuns)
	}
	if cfg.Server.RunTimeout != 60*time.Minute {
		t.Errorf("runTimeout = %v, want 60m", cfg.Server.RunTimeout)
	}
	if cfg.Server.Log.Level != "info" || cfg.Server.Log.Format != "json" {
		t.Errorf("log defaults = %+v", cfg.Server.Log)
	}
	if cfg.Executors[0].Ref != "main" {
		t.Errorf("ref default = %q, want main", cfg.Executors[0].Ref)
	}
	if cfg.Executors[0].PollInterval != 15*time.Second {
		t.Errorf("pollInterval default = %v, want 15s", cfg.Executors[0].PollInterval)
	}
}

func TestLoadIgnoresEnvVarsInComments(t *testing.T) {
	t.Setenv("TEST_GL_TOKEN", "x")
	t.Setenv("TEST_GL_SECRET", "y")
	commented := strings.Replace(validConfig, "server:",
		"# commented out: token: ${TOTALLY_UNSET_VAR}\nserver:", 1)
	if _, err := Load(writeConfig(t, commented)); err != nil {
		t.Fatalf("unset var in comment must not fail load: %v", err)
	}
}

func TestLoadUnsetEnvVarFails(t *testing.T) {
	t.Setenv("TEST_GL_TOKEN", "x")
	os.Unsetenv("TEST_GL_SECRET")
	_, err := Load(writeConfig(t, validConfig))
	if err == nil || !strings.Contains(err.Error(), "TEST_GL_SECRET") {
		t.Fatalf("want unset-var error naming TEST_GL_SECRET, got %v", err)
	}
}

func TestValidationRejectsBadResourceQuantity(t *testing.T) {
	t.Setenv("TEST_GL_TOKEN", "x")
	t.Setenv("TEST_GL_SECRET", "y")
	cfg := strings.Replace(validConfig, "rules:", `  - name: k8s
    type: kubernetes
    namespace: renovate
    image: renovate/renovate
    pod:
      resources:
        requests:
          cpu: "not-a-quantity"
rules:`, 1)
	_, err := Load(writeConfig(t, cfg))
	if err == nil || !strings.Contains(err.Error(), "invalid resource quantity") {
		t.Fatalf("want quantity error, got %v", err)
	}
}

func TestValidationErrors(t *testing.T) {
	t.Setenv("TEST_GL_TOKEN", "x")
	t.Setenv("TEST_GL_SECRET", "y")
	cases := []struct {
		name    string
		mutate  func(s string) string
		wantErr string
	}{
		{"no platforms", func(s string) string {
			return strings.Replace(s, "platforms:", "platformsX:", 1)
		}, "at least one platform"},
		{"unknown executor in rule", func(s string) string {
			return strings.Replace(s, "executor: ci", "executor: nope", 1)
		}, `unknown executor "nope"`},
		{"missing catch-all rule", func(s string) string {
			return strings.Replace(s, `match: "**"`, `match: "other/**"`, 1)
		}, "catch-all"},
		{"bad platform type", func(s string) string {
			return strings.Replace(s, "type: gitlab", "type: svn", 1)
		}, `platform type "svn"`},
		{"bad crontab", func(s string) string {
			return strings.Replace(s, `"0 3 * * *"`, `"not a cron"`, 1)
		}, "crontab"},
		{"bad timezone", func(s string) string {
			return strings.Replace(s, "Europe/Berlin", "Mars/Olympus", 1)
		}, "timezone"},
		{"missing webhook secret", func(s string) string {
			return strings.Replace(s, "secret: ${TEST_GL_SECRET}", "secret: \"\"", 1)
		}, "webhook secret"},
		{"pipeline executor references unknown platform", func(s string) string {
			return strings.Replace(s, "platform: gl\n", "platform: nope\n", 1)
		}, `unknown platform "nope"`},
		{"duplicate executor name", func(s string) string {
			return strings.Replace(s, "rules:",
				"  - name: ci\n    type: docker\n    image: renovate/renovate\nrules:", 1)
		}, "duplicate executor"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := Load(writeConfig(t, tc.mutate(validConfig)))
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("want error containing %q, got %v", tc.wantErr, err)
			}
		})
	}
}
