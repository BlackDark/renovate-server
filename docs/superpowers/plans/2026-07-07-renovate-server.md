# Renovate Server Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Build a slim Go server that coordinates Renovate runs on GitLab/GitHub repos via webhooks and cron, delegating execution to GitLab pipelines, Kubernetes Jobs, or Docker containers, with per-repo locking and coalescing.

**Architecture:** Single binary. Core dispatcher owns a per-repo state machine (idle→queued→running with pending-rerun coalescing), debounce, and a global semaphore, all behind a `Store` interface. Platform adapters (gitlab/github) normalize webhooks into events and discover repos; executor adapters run Renovate and block until completion. Rules route repos to executors, first match wins.

**Tech Stack:** Go 1.26, stdlib `net/http` + `log/slog`, `gitlab.com/gitlab-org/api/client-go v1.46.0`, `github.com/google/go-github/v76`, `k8s.io/client-go v0.36.2`, `github.com/docker/docker v28.5.2+incompatible`, `github.com/robfig/cron/v3`, `github.com/prometheus/client_golang v1.23.2`, `github.com/bmatcuk/doublestar/v4`, `gopkg.in/yaml.v3`.

**Spec:** `docs/superpowers/specs/2026-07-07-renovate-server-design.md`

## Global Constraints

- Module path: `github.com/BlackDark/renovate-server`. Go directive: `go 1.26`.
- Old code in the repo (pkg/, cmd/, cicd/, vendor/, scripts/, old dotfiles) is deleted in Task 1. Never reuse it.
- All code under `internal/` except `cmd/renovate-server`.
- Logging: `log/slog` only. No `fmt.Print*` outside `main` version output.
- Every package with logic gets tests; run `go test -race ./...` before every commit.
- Executors MUST honor `ctx` cancellation — the dispatcher's run timeout depends on it.
- Secrets never logged, never in the status API.
- Commits: conventional commits, no Co-Authored-By lines (user rule).
- `${VAR}` env expansion in config: pattern `${NAME}` only, unset var = load error.
- Webhook auth failures return 401; malformed payloads 400; ignorable events 200; accepted events 202.

---

### Task 1: Clean slate + module scaffold

**Files:**
- Delete: all old project files (see step 1)
- Create: `go.mod`, `.gitignore`, `.editorconfig`, `LICENSE`

**Interfaces:**
- Produces: empty Go module `github.com/BlackDark/renovate-server` that later tasks add packages to.

- [ ] **Step 1: Remove old project files**

```bash
cd /Users/marbaced/tmp/renovate-server
rm -rf pkg cmd cicd scripts vendor docs/development \
  go.mod go.sum Makefile README.md LICENSE.txt sonar-project.properties \
  .dukkha.yaml .ecrc .gitlab-ci.yml .golangci.yml .yaml-lint.yml \
  .pre-commit-config.yaml .renovaterc.json .github .gitattributes \
  .dockerignore .editorconfig .gitignore
```

Keep `docs/superpowers/` (spec + this plan).

- [ ] **Step 2: Init module and scaffold files**

```bash
go mod init github.com/BlackDark/renovate-server
```

Create `.gitignore`:

```gitignore
/renovate-server
/dist/
coverage.out
*.test
```

Create `.editorconfig`:

```ini
root = true

[*]
charset = utf-8
end_of_line = lf
insert_final_newline = true
trim_trailing_whitespace = true

[*.go]
indent_style = tab

[*.{yml,yaml,json,md}]
indent_style = space
indent_size = 2
```

Create `LICENSE` with the MIT license text, copyright line: `Copyright (c) 2026 Eduard Marbach`.

- [ ] **Step 3: Verify module builds (empty)**

Run: `go build ./...`
Expected: no output, exit 0.

- [ ] **Step 4: Commit**

```bash
git add -A
git commit -m "chore: remove legacy codebase, init go module"
```

---

### Task 2: Config package (load, env expansion, defaults, validation)

**Files:**
- Create: `internal/config/config.go`, `internal/config/load.go`
- Test: `internal/config/load_test.go`

**Interfaces:**
- Produces:
  - `config.Load(path string) (*Config, error)` — reads YAML, expands `${VAR}`, applies defaults, validates.
  - Types `Config`, `Server`, `Log`, `Platform`, `Webhook`, `Discovery`, `Schedule`, `Executor`, `Rule` (fields below — later tasks consume them verbatim).
  - Executor type constants: `"gitlabPipeline"`, `"kubernetes"`, `"docker"`. Platform type constants: `"gitlab"`, `"github"`.

- [ ] **Step 1: Write `internal/config/config.go` (types only, no logic — no test needed yet)**

```go
package config

import "time"

const (
	PlatformGitLab = "gitlab"
	PlatformGitHub = "github"

	ExecutorGitLabPipeline = "gitlabPipeline"
	ExecutorKubernetes     = "kubernetes"
	ExecutorDocker         = "docker"
)

type Config struct {
	Server    Server     `yaml:"server"`
	Platforms []Platform `yaml:"platforms"`
	Executors []Executor `yaml:"executors"`
	Rules     []Rule     `yaml:"rules"`
}

type Server struct {
	Listen            string        `yaml:"listen"`
	Log               Log           `yaml:"log"`
	Debounce          time.Duration `yaml:"debounce"`
	MaxConcurrentRuns int           `yaml:"maxConcurrentRuns"`
	RunTimeout        time.Duration `yaml:"runTimeout"`
}

type Log struct {
	Level  string `yaml:"level"`  // debug|info|warn|error
	Format string `yaml:"format"` // json|text
}

type Platform struct {
	Name      string    `yaml:"name"`
	Type      string    `yaml:"type"` // gitlab|github
	BaseURL   string    `yaml:"baseURL"`
	Token     string    `yaml:"token"`
	BotEmail  string    `yaml:"botEmail"` // push events from this author are ignored
	Webhook   Webhook   `yaml:"webhook"`
	Events    []string  `yaml:"events"` // merge_request|issue|push
	Discovery Discovery `yaml:"discovery"`
	Schedule  Schedule  `yaml:"schedule"`
}

type Webhook struct {
	Path   string `yaml:"path"`
	Secret string `yaml:"secret"`
}

type Discovery struct {
	Groups          []string `yaml:"groups"`
	ExcludeArchived bool     `yaml:"excludeArchived"`
}

type Schedule struct {
	Crontabs []string `yaml:"crontabs"`
	Timezone string   `yaml:"timezone"`
}

type Executor struct {
	Name string `yaml:"name"`
	Type string `yaml:"type"` // gitlabPipeline|kubernetes|docker

	// gitlabPipeline
	Platform     string            `yaml:"platform"` // references Platform.Name
	Project      string            `yaml:"project"`
	Ref          string            `yaml:"ref"`
	TriggerToken string            `yaml:"triggerToken"`
	Variables    map[string]string `yaml:"variables"` // values are Go templates: {{ .Repo }} {{ .Platform }} {{ .Reason }}
	PollInterval time.Duration     `yaml:"pollInterval"`

	// kubernetes
	Namespace string        `yaml:"namespace"`
	Image     string        `yaml:"image"` // also used by docker
	CachePVC  string        `yaml:"cachePVC"`
	JobTTL    time.Duration `yaml:"jobTTL"`

	// docker
	CacheVolume string `yaml:"cacheVolume"`
	Pull        bool   `yaml:"pull"`

	// kubernetes + docker: extra env for the renovate container
	Env map[string]string `yaml:"env"`
}

type Rule struct {
	Match    string `yaml:"match"`
	Executor string `yaml:"executor"`
	Disabled bool   `yaml:"disabled"`
}
```

- [ ] **Step 2: Write the failing tests `internal/config/load_test.go`**

```go
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

func TestLoadUnsetEnvVarFails(t *testing.T) {
	t.Setenv("TEST_GL_TOKEN", "x")
	os.Unsetenv("TEST_GL_SECRET")
	_, err := Load(writeConfig(t, validConfig))
	if err == nil || !strings.Contains(err.Error(), "TEST_GL_SECRET") {
		t.Fatalf("want unset-var error naming TEST_GL_SECRET, got %v", err)
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
			return strings.Replace(s, "platform: gl", "platform: nope", 1)
		}, `unknown platform "nope"`},
		{"duplicate executor name", func(s string) string {
			return s + "\n  - name: ci\n    type: docker\n    image: renovate/renovate\n"
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
```

Note: the "duplicate executor name" case appends to the executors list — YAML indentation in the appended string must match the `executors:` list (2 spaces). The `validConfig` string ends with the rules block, so instead append via `strings.Replace` on the executors section if the raw append proves brittle; the implementer may restructure that one case, keeping the assertion.

- [ ] **Step 3: Run tests to verify they fail**

Run: `go test ./internal/config/ -run . -v`
Expected: FAIL — `undefined: Load`.

- [ ] **Step 4: Implement `internal/config/load.go`**

```go
package config

import (
	"fmt"
	"os"
	"regexp"
	"time"

	"github.com/bmatcuk/doublestar/v4"
	"github.com/robfig/cron/v3"
	"gopkg.in/yaml.v3"
)

var envVarPattern = regexp.MustCompile(`\$\{([A-Za-z_][A-Za-z0-9_]*)\}`)

// Load reads the YAML config at path, expands ${VAR} references from the
// environment, applies defaults and validates. Any unset variable, unknown
// field or invalid reference is an error.
func Load(path string) (*Config, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read config: %w", err)
	}

	expanded, err := expandEnv(raw)
	if err != nil {
		return nil, err
	}

	var cfg Config
	if err := yaml.Unmarshal(expanded, &cfg); err != nil {
		return nil, fmt.Errorf("parse config: %w", err)
	}

	cfg.applyDefaults()
	if err := cfg.validate(); err != nil {
		return nil, fmt.Errorf("invalid config: %w", err)
	}
	return &cfg, nil
}

func expandEnv(raw []byte) ([]byte, error) {
	var missing []string
	out := envVarPattern.ReplaceAllFunc(raw, func(m []byte) []byte {
		name := string(envVarPattern.FindSubmatch(m)[1])
		val, ok := os.LookupEnv(name)
		if !ok {
			missing = append(missing, name)
			return m
		}
		return []byte(val)
	})
	if len(missing) > 0 {
		return nil, fmt.Errorf("unset environment variables referenced in config: %v", missing)
	}
	return out, nil
}

func (c *Config) applyDefaults() {
	if c.Server.Listen == "" {
		c.Server.Listen = ":8080"
	}
	if c.Server.Log.Level == "" {
		c.Server.Log.Level = "info"
	}
	if c.Server.Log.Format == "" {
		c.Server.Log.Format = "json"
	}
	if c.Server.Debounce == 0 {
		c.Server.Debounce = 10 * time.Second
	}
	if c.Server.MaxConcurrentRuns == 0 {
		c.Server.MaxConcurrentRuns = 4
	}
	if c.Server.RunTimeout == 0 {
		c.Server.RunTimeout = 60 * time.Minute
	}
	for i := range c.Executors {
		e := &c.Executors[i]
		if e.Type == ExecutorGitLabPipeline {
			if e.Ref == "" {
				e.Ref = "main"
			}
			if e.PollInterval == 0 {
				e.PollInterval = 15 * time.Second
			}
		}
		if e.Type == ExecutorKubernetes && e.JobTTL == 0 {
			e.JobTTL = time.Hour
		}
	}
}

func (c *Config) validate() error {
	if len(c.Platforms) == 0 {
		return fmt.Errorf("at least one platform must be configured")
	}

	platformNames := map[string]string{} // name -> type
	for i, p := range c.Platforms {
		if p.Name == "" {
			return fmt.Errorf("platform %d: name is required", i)
		}
		if _, dup := platformNames[p.Name]; dup {
			return fmt.Errorf("duplicate platform name %q", p.Name)
		}
		if p.Type != PlatformGitLab && p.Type != PlatformGitHub {
			return fmt.Errorf("platform %q: platform type %q is not supported", p.Name, p.Type)
		}
		if p.Token == "" {
			return fmt.Errorf("platform %q: token is required", p.Name)
		}
		if p.Webhook.Path == "" {
			return fmt.Errorf("platform %q: webhook path is required", p.Name)
		}
		if p.Webhook.Secret == "" {
			return fmt.Errorf("platform %q: webhook secret is required", p.Name)
		}
		for _, ev := range p.Events {
			switch ev {
			case "merge_request", "issue", "push":
			default:
				return fmt.Errorf("platform %q: unknown event type %q", p.Name, ev)
			}
		}
		if p.Schedule.Timezone != "" {
			if _, err := time.LoadLocation(p.Schedule.Timezone); err != nil {
				return fmt.Errorf("platform %q: invalid timezone %q", p.Name, p.Schedule.Timezone)
			}
		}
		for _, tab := range p.Schedule.Crontabs {
			if _, err := cron.ParseStandard(tab); err != nil {
				return fmt.Errorf("platform %q: invalid crontab %q: %w", p.Name, tab, err)
			}
		}
		platformNames[p.Name] = p.Type
	}

	executorNames := map[string]bool{}
	for i, e := range c.Executors {
		if e.Name == "" {
			return fmt.Errorf("executor %d: name is required", i)
		}
		if executorNames[e.Name] {
			return fmt.Errorf("duplicate executor name %q", e.Name)
		}
		executorNames[e.Name] = true
		switch e.Type {
		case ExecutorGitLabPipeline:
			typ, ok := platformNames[e.Platform]
			if !ok {
				return fmt.Errorf("executor %q: unknown platform %q", e.Name, e.Platform)
			}
			if typ != PlatformGitLab {
				return fmt.Errorf("executor %q: platform %q is not a gitlab platform", e.Name, e.Platform)
			}
			if e.Project == "" {
				return fmt.Errorf("executor %q: project is required", e.Name)
			}
			if e.TriggerToken == "" {
				return fmt.Errorf("executor %q: triggerToken is required", e.Name)
			}
		case ExecutorKubernetes:
			if e.Namespace == "" {
				return fmt.Errorf("executor %q: namespace is required", e.Name)
			}
			if e.Image == "" {
				return fmt.Errorf("executor %q: image is required", e.Name)
			}
		case ExecutorDocker:
			if e.Image == "" {
				return fmt.Errorf("executor %q: image is required", e.Name)
			}
		default:
			return fmt.Errorf("executor %q: executor type %q is not supported", e.Name, e.Type)
		}
	}

	if len(c.Rules) == 0 {
		return fmt.Errorf("at least one rule is required")
	}
	hasCatchAll := false
	for i, r := range c.Rules {
		if !doublestar.ValidatePattern(r.Match) {
			return fmt.Errorf("rule %d: invalid glob pattern %q", i, r.Match)
		}
		if r.Match == "**" {
			hasCatchAll = true
		}
		if r.Disabled {
			continue
		}
		if !executorNames[r.Executor] {
			return fmt.Errorf("rule %d: unknown executor %q", i, r.Executor)
		}
	}
	if !hasCatchAll {
		return fmt.Errorf(`rules must include a catch-all rule with match: "**"`)
	}
	return nil
}
```

- [ ] **Step 5: Add dependencies and run tests**

```bash
go get gopkg.in/yaml.v3@v3.0.1 github.com/bmatcuk/doublestar/v4@latest github.com/robfig/cron/v3@v3.0.1
go test -race ./internal/config/
```
Expected: PASS.

- [ ] **Step 6: Commit**

```bash
git add internal/config go.mod go.sum
git commit -m "feat: add config loading with env expansion and validation"
```

---

### Task 3: Platform types + checkbox detection

**Files:**
- Create: `internal/platform/platform.go`, `internal/platform/checkbox.go`
- Test: `internal/platform/checkbox_test.go`

**Interfaces:**
- Produces (consumed by dispatch, server, platform adapters, executors):

```go
type Repo struct {
	Platform string // platform config name
	FullName string // e.g. "group/subgroup/project"
}
func (r Repo) Key() string // "<Platform>:<FullName>"

type Reason string
const (
	ReasonMergeRequest Reason = "merge_request"
	ReasonIssue        Reason = "issue"
	ReasonPush         Reason = "push"
	ReasonCron         Reason = "cron"
	ReasonRerun        Reason = "rerun"
)

type Event struct {
	Repo   Repo
	Reason Reason
}

var ErrUnauthorized = errors.New("webhook authentication failed")

type Platform interface {
	Name() string
	WebhookPath() string
	// ParseWebhook authenticates and parses a webhook request. body is the
	// already-read request body. Returns (nil, nil) when the event needs no
	// action, ErrUnauthorized when authentication fails.
	ParseWebhook(r *http.Request, body []byte) (*Event, error)
	DiscoverRepos(ctx context.Context) ([]Repo, error)
	Schedule() config.Schedule
}

func CheckedItems(text string) int
```

- [ ] **Step 1: Write the failing test `internal/platform/checkbox_test.go`**

```go
package platform

import "testing"

func TestCheckedItems(t *testing.T) {
	cases := []struct {
		name string
		text string
		want int
	}{
		{"empty", "", 0},
		{"unchecked only", "- [ ] rebase\n- [ ] retry", 0},
		{"one checked dash", "- [x] rebase", 1},
		{"uppercase X", "- [X] rebase", 1},
		{"asterisk list", "* [x] item", 1},
		{"numbered list", "1. [x] item", 1},
		{"indented", "  - [x] nested", 1},
		{"mixed", "- [x] a\n- [ ] b\n- [x] c", 2},
		{"not a list item", "text [x] inline", 0},
		{"checkbox mid-line", "- foo [x] bar", 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := CheckedItems(tc.text); got != tc.want {
				t.Errorf("CheckedItems(%q) = %d, want %d", tc.text, got, tc.want)
			}
		})
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/platform/ -v`
Expected: FAIL — `undefined: CheckedItems`.

- [ ] **Step 3: Implement**

`internal/platform/checkbox.go`:

```go
package platform

import "regexp"

var checkedItem = regexp.MustCompile(`(?mi)^\s*(?:[-*+]|\d+\.)\s+\[x\]`)

// CheckedItems counts checked markdown todo items ("- [x] ...") in text.
// Used to detect Renovate checkbox ticks in MR/issue descriptions: a tick
// is a transition where the checked count increases.
func CheckedItems(text string) int {
	return len(checkedItem.FindAllString(text, -1))
}
```

`internal/platform/platform.go`:

```go
package platform

import (
	"context"
	"errors"
	"net/http"

	"github.com/BlackDark/renovate-server/internal/config"
)

// Repo identifies a repository on a configured platform.
type Repo struct {
	Platform string // platform config name
	FullName string // e.g. "group/subgroup/project"
}

// Key returns the unique dispatch key for the repo.
func (r Repo) Key() string { return r.Platform + ":" + r.FullName }

// Reason describes why a run was requested.
type Reason string

const (
	ReasonMergeRequest Reason = "merge_request"
	ReasonIssue        Reason = "issue"
	ReasonPush         Reason = "push"
	ReasonCron         Reason = "cron"
	ReasonRerun        Reason = "rerun"
)

// Event is a normalized trigger extracted from a webhook or schedule.
type Event struct {
	Repo   Repo
	Reason Reason
}

// ErrUnauthorized is returned by ParseWebhook when authentication fails.
var ErrUnauthorized = errors.New("webhook authentication failed")

// Platform abstracts a git hosting platform.
type Platform interface {
	Name() string
	WebhookPath() string
	// ParseWebhook authenticates and parses a webhook request. body is the
	// already-read request body. Returns (nil, nil) when the event needs no
	// action, ErrUnauthorized when authentication fails.
	ParseWebhook(r *http.Request, body []byte) (*Event, error)
	// DiscoverRepos lists all repos under the configured groups/orgs.
	DiscoverRepos(ctx context.Context) ([]Repo, error)
	// Schedule returns the cron schedule config for this platform.
	Schedule() config.Schedule
}
```

- [ ] **Step 4: Run tests**

Run: `go test -race ./internal/platform/`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/platform
git commit -m "feat: add platform types and checkbox detection"
```

---

### Task 4: Store interface + memory implementation

**Files:**
- Create: `internal/store/store.go`, `internal/store/memory.go`
- Test: `internal/store/memory_test.go`

**Interfaces:**
- Produces (consumed by dispatch, server):

```go
type State string
const (
	StateQueued  State = "queued"
	StateRunning State = "running"
)

type QueueResult int
const (
	Queued    QueueResult = iota // was idle; caller must schedule a run
	Coalesced                    // already queued; merged into pending run
	Deferred                     // running; rerun flagged for after completion
)

type RepoStatus struct {
	State        State     `json:"state"`
	Reason       string    `json:"reason"`
	Since        time.Time `json:"since"`
	PendingRerun bool      `json:"pendingRerun"`
}

type Store interface {
	Queue(key, reason string) QueueResult
	StartRun(key string)
	FinishRun(key string) (rerun bool)
	Adopt(key, reason string) // mark running directly (restart re-adoption)
	Snapshot() map[string]RepoStatus
}

func NewMemory() Store
```

Idle repos have no entry — `FinishRun` with no pending rerun deletes the entry (bounded memory).

- [ ] **Step 1: Write the failing test `internal/store/memory_test.go`**

```go
package store

import (
	"sync"
	"testing"
)

func TestQueueTransitions(t *testing.T) {
	s := NewMemory()

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
}

func TestSnapshot(t *testing.T) {
	s := NewMemory()
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
}

func TestAdopt(t *testing.T) {
	s := NewMemory()
	s.Adopt("gl:a", "adopted")
	if snap := s.Snapshot(); snap["gl:a"].State != StateRunning {
		t.Fatalf("adopted repo state = %+v", snap["gl:a"])
	}
	if got := s.Queue("gl:a", "push"); got != Deferred {
		t.Fatalf("Queue on adopted = %v, want Deferred", got)
	}
}

func TestConcurrentAccess(t *testing.T) {
	s := NewMemory()
	var wg sync.WaitGroup
	for i := 0; i < 50; i++ {
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
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/store/ -v`
Expected: FAIL — `undefined: NewMemory`.

- [ ] **Step 3: Implement**

`internal/store/store.go`:

```go
package store

import "time"

// State of a tracked repo. Idle repos are not tracked.
type State string

const (
	StateQueued  State = "queued"
	StateRunning State = "running"
)

// QueueResult tells the caller what a Queue call did.
type QueueResult int

const (
	// Queued: repo was idle, caller must schedule a run.
	Queued QueueResult = iota
	// Coalesced: repo already queued, event merged into the pending run.
	Coalesced
	// Deferred: repo is running, a rerun was flagged for after completion.
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
}
```

`internal/store/memory.go`:

```go
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
```

- [ ] **Step 4: Run tests**

Run: `go test -race ./internal/store/`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/store
git commit -m "feat: add repo state store with in-memory implementation"
```

---

### Task 5: Executor interface + rule router

**Files:**
- Create: `internal/executor/executor.go`, `internal/dispatch/router.go`
- Test: `internal/dispatch/router_test.go`

**Interfaces:**
- Produces:

```go
// internal/executor
type RunSpec struct {
	Repo   platform.Repo
	Reason platform.Reason
}

type Executor interface {
	Name() string
	// Run executes renovate for spec and blocks until the run finishes.
	// Implementations MUST honor ctx cancellation (run timeout).
	Run(ctx context.Context, spec RunSpec) error
}

// Adoptable is implemented by executors that can re-attach to runs already
// in flight after a server restart (kubernetes).
type Adoptable interface {
	AdoptRunning(ctx context.Context) ([]AdoptedRun, error)
}

type AdoptedRun struct {
	Repo platform.Repo
	Wait func(ctx context.Context) error
}

// internal/dispatch
type Route struct {
	Executor executor.Executor // nil iff Disabled
	Disabled bool
}
func NewRouter(rules []config.Rule, executors map[string]executor.Executor) (*Router, error)
func (r *Router) Route(repoFullName string) Route
```

- [ ] **Step 1: Write `internal/executor/executor.go` (types only)**

```go
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
```

- [ ] **Step 2: Write the failing test `internal/dispatch/router_test.go`**

```go
package dispatch

import (
	"context"
	"testing"

	"github.com/BlackDark/renovate-server/internal/config"
	"github.com/BlackDark/renovate-server/internal/executor"
)

type fakeExecutor struct{ name string }

func (f *fakeExecutor) Name() string                                        { return f.name }
func (f *fakeExecutor) Run(context.Context, executor.RunSpec) error         { return nil }

func TestRouterFirstMatchWins(t *testing.T) {
	ci := &fakeExecutor{name: "ci"}
	k8s := &fakeExecutor{name: "k8s"}
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
```

- [ ] **Step 3: Run test to verify it fails**

Run: `go test ./internal/dispatch/ -v`
Expected: FAIL — `undefined: NewRouter`.

- [ ] **Step 4: Implement `internal/dispatch/router.go`**

```go
package dispatch

import (
	"fmt"

	"github.com/bmatcuk/doublestar/v4"

	"github.com/BlackDark/renovate-server/internal/config"
	"github.com/BlackDark/renovate-server/internal/executor"
)

// Route is the outcome of matching a repo against the rules.
type Route struct {
	Executor executor.Executor // nil iff Disabled
	Disabled bool
}

type compiledRule struct {
	pattern  string
	disabled bool
	exec     executor.Executor
}

// Router matches repo full names against ordered rules, first match wins.
// Config validation guarantees a catch-all rule exists.
type Router struct {
	rules []compiledRule
}

func NewRouter(rules []config.Rule, executors map[string]executor.Executor) (*Router, error) {
	r := &Router{}
	for i, rule := range rules {
		cr := compiledRule{pattern: rule.Match, disabled: rule.Disabled}
		if !rule.Disabled {
			exec, ok := executors[rule.Executor]
			if !ok {
				return nil, fmt.Errorf("rule %d: unknown executor %q", i, rule.Executor)
			}
			cr.exec = exec
		}
		r.rules = append(r.rules, cr)
	}
	return r, nil
}

func (r *Router) Route(repoFullName string) Route {
	for _, rule := range r.rules {
		ok, err := doublestar.Match(rule.pattern, repoFullName)
		if err != nil || !ok {
			continue
		}
		return Route{Executor: rule.exec, Disabled: rule.disabled}
	}
	// Unreachable with a validated config (catch-all required); treat as disabled.
	return Route{Disabled: true}
}
```

- [ ] **Step 5: Run tests**

Run: `go test -race ./internal/dispatch/ ./internal/executor/`
Expected: PASS.

- [ ] **Step 6: Commit**

```bash
git add internal/executor internal/dispatch
git commit -m "feat: add executor interface and rule-based router"
```

---

### Task 6: Dispatcher (debounce, coalescing, semaphore, timeout)

**Files:**
- Create: `internal/dispatch/dispatcher.go`
- Test: `internal/dispatch/dispatcher_test.go`

**Interfaces:**
- Consumes: `store.Store`, `Router`, `executor.Executor`, `platform.Event`.
- Produces (consumed by server, schedule, main):

```go
type Metrics interface {
	RunStarted(executorName string)
	RunFinished(executorName, result string, seconds float64) // result: success|failure|timeout
}

func NewDispatcher(st store.Store, router *Router, opts Options) *Dispatcher

type Options struct {
	Debounce      time.Duration
	RunTimeout    time.Duration
	MaxConcurrent int
	Log           *slog.Logger
	Metrics       Metrics // may be nil
}

func (d *Dispatcher) Enqueue(ev platform.Event)
func (d *Dispatcher) Adopt(run executor.AdoptedRun, executorName string)
func (d *Dispatcher) Shutdown(ctx context.Context) error // waits for in-flight runs
```

- [ ] **Step 1: Write the failing test `internal/dispatch/dispatcher_test.go`**

```go
package dispatch

import (
	"context"
	"log/slog"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/BlackDark/renovate-server/internal/config"
	"github.com/BlackDark/renovate-server/internal/executor"
	"github.com/BlackDark/renovate-server/internal/platform"
	"github.com/BlackDark/renovate-server/internal/store"
)

// blockingExecutor records runs and blocks each until released.
type blockingExecutor struct {
	mu      sync.Mutex
	runs    []executor.RunSpec
	release chan struct{}
	active  atomic.Int32
	maxSeen atomic.Int32
}

func newBlockingExecutor() *blockingExecutor {
	return &blockingExecutor{release: make(chan struct{})}
}

func (b *blockingExecutor) Name() string { return "fake" }

func (b *blockingExecutor) Run(ctx context.Context, spec executor.RunSpec) error {
	b.mu.Lock()
	b.runs = append(b.runs, spec)
	b.mu.Unlock()
	n := b.active.Add(1)
	for {
		prev := b.maxSeen.Load()
		if n <= prev || b.maxSeen.CompareAndSwap(prev, n) {
			break
		}
	}
	defer b.active.Add(-1)
	select {
	case <-b.release:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (b *blockingExecutor) runCount() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return len(b.runs)
}

func testDispatcher(t *testing.T, exec executor.Executor, opts Options) *Dispatcher {
	t.Helper()
	router, err := NewRouter(
		[]config.Rule{{Match: "**", Executor: "fake"}},
		map[string]executor.Executor{"fake": exec},
	)
	if err != nil {
		t.Fatal(err)
	}
	if opts.Log == nil {
		opts.Log = slog.New(slog.DiscardHandler)
	}
	if opts.Debounce == 0 {
		opts.Debounce = time.Millisecond
	}
	if opts.RunTimeout == 0 {
		opts.RunTimeout = time.Minute
	}
	if opts.MaxConcurrent == 0 {
		opts.MaxConcurrent = 4
	}
	return NewDispatcher(store.NewMemory(), router, opts)
}

func event(name string) platform.Event {
	return platform.Event{
		Repo:   platform.Repo{Platform: "gl", FullName: name},
		Reason: platform.ReasonPush,
	}
}

func waitFor(t *testing.T, cond func() bool, msg string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatal("timeout waiting for: " + msg)
}

func TestDebounceCoalescesEvents(t *testing.T) {
	exec := newBlockingExecutor()
	d := testDispatcher(t, exec, Options{Debounce: 50 * time.Millisecond})
	for i := 0; i < 10; i++ {
		d.Enqueue(event("g/a"))
	}
	waitFor(t, func() bool { return exec.runCount() == 1 }, "one run")
	close(exec.release)
	shutdown(t, d)
	if got := exec.runCount(); got != 1 {
		t.Fatalf("runs = %d, want 1", got)
	}
}

func TestEventDuringRunTriggersExactlyOneRerun(t *testing.T) {
	exec := newBlockingExecutor()
	d := testDispatcher(t, exec, Options{})
	d.Enqueue(event("g/a"))
	waitFor(t, func() bool { return exec.active.Load() == 1 }, "run started")

	// events while running: all coalesce into one rerun
	d.Enqueue(event("g/a"))
	d.Enqueue(event("g/a"))
	d.Enqueue(event("g/a"))

	close(exec.release) // releases current and all future runs
	waitFor(t, func() bool { return exec.runCount() == 2 }, "rerun happened")
	shutdown(t, d)
	if got := exec.runCount(); got != 2 {
		t.Fatalf("runs = %d, want 2 (original + one rerun)", got)
	}
}

func TestGlobalConcurrencyLimit(t *testing.T) {
	exec := newBlockingExecutor()
	d := testDispatcher(t, exec, Options{MaxConcurrent: 2})
	for _, r := range []string{"g/a", "g/b", "g/c", "g/d", "g/e"} {
		d.Enqueue(event(r))
	}
	waitFor(t, func() bool { return exec.active.Load() == 2 }, "2 active")
	time.Sleep(20 * time.Millisecond) // give extras a chance to (wrongly) start
	if got := exec.maxSeen.Load(); got != 2 {
		t.Fatalf("max concurrent = %d, want 2", got)
	}
	close(exec.release)
	shutdown(t, d)
	if got := exec.runCount(); got != 5 {
		t.Fatalf("runs = %d, want 5", got)
	}
}

func TestRunTimeoutReleasesLock(t *testing.T) {
	exec := newBlockingExecutor() // never released -> only ctx ends runs
	d := testDispatcher(t, exec, Options{RunTimeout: 30 * time.Millisecond})
	d.Enqueue(event("g/a"))
	waitFor(t, func() bool { return exec.runCount() == 1 && exec.active.Load() == 0 }, "timed-out run finished")
	// lock released: a new event can run again
	d.Enqueue(event("g/a"))
	waitFor(t, func() bool { return exec.runCount() == 2 }, "second run after timeout")
	shutdown(t, d)
}

func TestDisabledRepoNeverRuns(t *testing.T) {
	exec := newBlockingExecutor()
	router, err := NewRouter([]config.Rule{
		{Match: "g/off/**", Disabled: true},
		{Match: "**", Executor: "fake"},
	}, map[string]executor.Executor{"fake": exec})
	if err != nil {
		t.Fatal(err)
	}
	d := NewDispatcher(store.NewMemory(), router, Options{
		Debounce: time.Millisecond, RunTimeout: time.Minute, MaxConcurrent: 1,
		Log: slog.New(slog.DiscardHandler),
	})
	d.Enqueue(event("g/off/app"))
	time.Sleep(30 * time.Millisecond)
	if exec.runCount() != 0 {
		t.Fatalf("disabled repo ran %d times", exec.runCount())
	}
	shutdown(t, d)
}

func TestAdoptedRunBlocksNewRunsUntilDone(t *testing.T) {
	exec := newBlockingExecutor()
	d := testDispatcher(t, exec, Options{})
	adoptedDone := make(chan struct{})
	d.Adopt(executor.AdoptedRun{
		Repo: platform.Repo{Platform: "gl", FullName: "g/a"},
		Wait: func(ctx context.Context) error {
			select {
			case <-adoptedDone:
				return nil
			case <-ctx.Done():
				return ctx.Err()
			}
		},
	}, "fake")

	d.Enqueue(event("g/a")) // must defer, not start
	time.Sleep(20 * time.Millisecond)
	if exec.runCount() != 0 {
		t.Fatal("run started while adopted run in flight")
	}
	close(adoptedDone)
	close(exec.release)
	waitFor(t, func() bool { return exec.runCount() == 1 }, "deferred run after adoption finished")
	shutdown(t, d)
}

func shutdown(t *testing.T, d *Dispatcher) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := d.Shutdown(ctx); err != nil {
		t.Fatalf("shutdown: %v", err)
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/dispatch/ -v`
Expected: FAIL — `undefined: NewDispatcher`, `undefined: Options`.

- [ ] **Step 3: Implement `internal/dispatch/dispatcher.go`**

```go
package dispatch

import (
	"context"
	"errors"
	"log/slog"
	"sync"
	"time"

	"github.com/BlackDark/renovate-server/internal/executor"
	"github.com/BlackDark/renovate-server/internal/platform"
	"github.com/BlackDark/renovate-server/internal/store"
)

// Metrics receives run lifecycle notifications. Implemented by the metrics
// package; nil-safe via the noop default.
type Metrics interface {
	RunStarted(executorName string)
	RunFinished(executorName, result string, seconds float64)
}

type noopMetrics struct{}

func (noopMetrics) RunStarted(string)                  {}
func (noopMetrics) RunFinished(string, string, float64) {}

// Options configures a Dispatcher.
type Options struct {
	Debounce      time.Duration
	RunTimeout    time.Duration
	MaxConcurrent int
	Log           *slog.Logger
	Metrics       Metrics
}

// Dispatcher owns the per-repo run lifecycle: debounce, mutual exclusion,
// rerun coalescing, global concurrency and the run timeout.
type Dispatcher struct {
	store   store.Store
	router  *Router
	opts    Options
	sem     chan struct{}
	wg      sync.WaitGroup
	baseCtx context.Context
	cancel  context.CancelFunc
}

func NewDispatcher(st store.Store, router *Router, opts Options) *Dispatcher {
	if opts.Log == nil {
		opts.Log = slog.Default()
	}
	if opts.Metrics == nil {
		opts.Metrics = noopMetrics{}
	}
	ctx, cancel := context.WithCancel(context.Background())
	return &Dispatcher{
		store:   st,
		router:  router,
		opts:    opts,
		sem:     make(chan struct{}, opts.MaxConcurrent),
		baseCtx: ctx,
		cancel:  cancel,
	}
}

// Enqueue requests a run for the event's repo. Duplicate requests coalesce:
// one pending run while queued, one deferred rerun while running.
func (d *Dispatcher) Enqueue(ev platform.Event) {
	log := d.opts.Log.With("repo", ev.Repo.Key(), "reason", string(ev.Reason))
	route := d.router.Route(ev.Repo.FullName)
	if route.Disabled {
		log.Debug("repo disabled by rules, ignoring")
		return
	}

	switch d.store.Queue(ev.Repo.Key(), string(ev.Reason)) {
	case store.Queued:
		log.Info("run queued")
		d.wg.Add(1)
		go d.run(ev, route.Executor)
	case store.Coalesced:
		log.Debug("event coalesced into queued run")
	case store.Deferred:
		log.Info("run in flight, rerun scheduled")
	}
}

// Adopt registers an in-flight run discovered at startup. The repo is
// locked until wait returns; a deferred rerun fires afterwards if events
// arrived meanwhile.
func (d *Dispatcher) Adopt(run executor.AdoptedRun, executorName string) {
	key := run.Repo.Key()
	d.store.Adopt(key, string(platform.ReasonRerun))
	d.opts.Log.Info("adopted in-flight run", "repo", key, "executor", executorName)
	d.wg.Add(1)
	go func() {
		defer d.wg.Done()
		ctx, cancel := context.WithTimeout(d.baseCtx, d.opts.RunTimeout)
		defer cancel()
		err := run.Wait(ctx)
		d.finish(run.Repo, executorName, time.Now(), err)
	}()
}

func (d *Dispatcher) run(ev platform.Event, exec executor.Executor) {
	defer d.wg.Done()

	select {
	case <-time.After(d.opts.Debounce):
	case <-d.baseCtx.Done():
		d.store.FinishRun(ev.Repo.Key())
		return
	}

	select {
	case d.sem <- struct{}{}:
	case <-d.baseCtx.Done():
		d.store.FinishRun(ev.Repo.Key())
		return
	}
	defer func() { <-d.sem }()

	d.store.StartRun(ev.Repo.Key())
	d.opts.Metrics.RunStarted(exec.Name())
	start := time.Now()

	ctx, cancel := context.WithTimeout(d.baseCtx, d.opts.RunTimeout)
	defer cancel()
	err := exec.Run(ctx, executor.RunSpec{Repo: ev.Repo, Reason: ev.Reason})
	d.finish(ev.Repo, exec.Name(), start, err)
}

func (d *Dispatcher) finish(repo platform.Repo, executorName string, start time.Time, err error) {
	log := d.opts.Log.With("repo", repo.Key(), "executor", executorName)
	result := "success"
	switch {
	case err == nil:
		log.Info("run finished", "duration", time.Since(start))
	case errors.Is(err, context.DeadlineExceeded):
		result = "timeout"
		log.Error("run timed out, lock released", "timeout", d.opts.RunTimeout)
	default:
		result = "failure"
		log.Error("run failed", "error", err)
	}
	d.opts.Metrics.RunFinished(executorName, result, time.Since(start).Seconds())

	if rerun := d.store.FinishRun(repo.Key()); rerun {
		log.Info("deferred rerun triggered")
		d.Enqueue(platform.Event{Repo: repo, Reason: platform.ReasonRerun})
	}
}

// Shutdown stops accepting timed work and waits for in-flight runs.
func (d *Dispatcher) Shutdown(ctx context.Context) error {
	done := make(chan struct{})
	go func() {
		d.wg.Wait()
		close(done)
	}()
	select {
	case <-done:
		d.cancel()
		return nil
	case <-ctx.Done():
		d.cancel()
		return ctx.Err()
	}
}
```

Note on `TestRunTimeoutReleasesLock`: after a timeout the deferred-rerun flag is NOT set (no event arrived), so the repo returns to idle — the second `Enqueue` starts a fresh run. Failed runs are not auto-retried by design.

- [ ] **Step 4: Run tests (race detector mandatory here)**

Run: `go test -race -count=3 ./internal/dispatch/`
Expected: PASS, no races.

- [ ] **Step 5: Commit**

```bash
git add internal/dispatch
git commit -m "feat: add dispatcher with debounce, coalescing and run timeout"
```

---

### Task 7: GitLab platform adapter

**Files:**
- Create: `internal/platform/gitlab/gitlab.go`
- Test: `internal/platform/gitlab/gitlab_test.go`

**Interfaces:**
- Consumes: `config.Platform`, `platform.Platform` interface, `platform.CheckedItems`.
- Produces: `gitlab.New(cfg config.Platform, log *slog.Logger) (*GitLab, error)` implementing `platform.Platform`. Exposes `Client() *gogitlab.Client` for the gitlabci executor (Task 9) to reuse the authenticated API client.

**Event semantics:**
- Merge request event: trigger when checked-checkbox count increased (`changes.description` previous→current). No `changes.description.current` → no trigger.
- Issue event: same delta rule on the issue description.
- Push event: only if `push` is in configured events, ref is the project default branch, and `user_email` differs from `botEmail`.
- Events not in the configured `events` list are ignored (nil, nil).

- [ ] **Step 1: Write the failing test `internal/platform/gitlab/gitlab_test.go`**

```go
package gitlab

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/BlackDark/renovate-server/internal/config"
	"github.com/BlackDark/renovate-server/internal/platform"
)

func testConfig(baseURL string) config.Platform {
	return config.Platform{
		Name:     "gl",
		Type:     config.PlatformGitLab,
		BaseURL:  baseURL,
		Token:    "glpat-test",
		BotEmail: "renovate@example.com",
		Webhook:  config.Webhook{Path: "/webhook/gitlab", Secret: "s3cret"},
		Events:   []string{"merge_request", "issue", "push"},
		Discovery: config.Discovery{
			Groups:          []string{"top-group"},
			ExcludeArchived: true,
		},
	}
}

func newTestPlatform(t *testing.T, baseURL string) *GitLab {
	t.Helper()
	g, err := New(testConfig(baseURL), slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatal(err)
	}
	return g
}

func webhookRequest(eventType, token string, body string) *http.Request {
	r := httptest.NewRequest(http.MethodPost, "/webhook/gitlab", bytes.NewBufferString(body))
	r.Header.Set("X-Gitlab-Event", eventType)
	r.Header.Set("X-Gitlab-Token", token)
	return r
}

const mrTicked = `{
  "object_kind": "merge_request",
  "project": {"path_with_namespace": "top-group/app", "default_branch": "main"},
  "object_attributes": {"iid": 7, "action": "update", "description": "- [x] rebase"},
  "changes": {"description": {"previous": "- [ ] rebase", "current": "- [x] rebase"}}
}`

const mrUnticked = `{
  "object_kind": "merge_request",
  "project": {"path_with_namespace": "top-group/app", "default_branch": "main"},
  "object_attributes": {"iid": 7, "action": "update", "description": "- [ ] rebase"},
  "changes": {"description": {"previous": "- [x] rebase", "current": "- [ ] rebase"}}
}`

const mrNoDescriptionChange = `{
  "object_kind": "merge_request",
  "project": {"path_with_namespace": "top-group/app", "default_branch": "main"},
  "object_attributes": {"iid": 7, "action": "update", "description": "- [x] rebase"},
  "changes": {}
}`

const issueTicked = `{
  "object_kind": "issue",
  "project": {"path_with_namespace": "top-group/app", "default_branch": "main"},
  "object_attributes": {"iid": 1, "action": "update", "title": "Dependency Dashboard", "description": "- [x] approve all"},
  "changes": {"description": {"previous": "- [ ] approve all", "current": "- [x] approve all"}}
}`

const pushByHuman = `{
  "object_kind": "push",
  "ref": "refs/heads/main",
  "user_email": "dev@example.com",
  "project": {"path_with_namespace": "top-group/app", "default_branch": "main"}
}`

const pushByBot = `{
  "object_kind": "push",
  "ref": "refs/heads/main",
  "user_email": "renovate@example.com",
  "project": {"path_with_namespace": "top-group/app", "default_branch": "main"}
}`

const pushFeatureBranch = `{
  "object_kind": "push",
  "ref": "refs/heads/feature-x",
  "user_email": "dev@example.com",
  "project": {"path_with_namespace": "top-group/app", "default_branch": "main"}
}`

func TestParseWebhookAuth(t *testing.T) {
	g := newTestPlatform(t, "https://gitlab.example.com")
	r := webhookRequest("Merge Request Hook", "wrong-secret", mrTicked)
	_, err := g.ParseWebhook(r, []byte(mrTicked))
	if !errors.Is(err, platform.ErrUnauthorized) {
		t.Fatalf("want ErrUnauthorized, got %v", err)
	}
}

func TestParseWebhookEvents(t *testing.T) {
	g := newTestPlatform(t, "https://gitlab.example.com")
	cases := []struct {
		name      string
		eventType string
		body      string
		want      *platform.Event // nil = no action
	}{
		{"mr checkbox ticked", "Merge Request Hook", mrTicked, &platform.Event{
			Repo:   platform.Repo{Platform: "gl", FullName: "top-group/app"},
			Reason: platform.ReasonMergeRequest,
		}},
		{"mr checkbox unticked", "Merge Request Hook", mrUnticked, nil},
		{"mr without description change", "Merge Request Hook", mrNoDescriptionChange, nil},
		{"issue checkbox ticked", "Issue Hook", issueTicked, &platform.Event{
			Repo:   platform.Repo{Platform: "gl", FullName: "top-group/app"},
			Reason: platform.ReasonIssue,
		}},
		{"push by human to default branch", "Push Hook", pushByHuman, &platform.Event{
			Repo:   platform.Repo{Platform: "gl", FullName: "top-group/app"},
			Reason: platform.ReasonPush,
		}},
		{"push by bot ignored", "Push Hook", pushByBot, nil},
		{"push to feature branch ignored", "Push Hook", pushFeatureBranch, nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := webhookRequest(tc.eventType, "s3cret", tc.body)
			got, err := g.ParseWebhook(r, []byte(tc.body))
			if err != nil {
				t.Fatalf("ParseWebhook: %v", err)
			}
			if tc.want == nil {
				if got != nil {
					t.Fatalf("want no event, got %+v", got)
				}
				return
			}
			if got == nil || *got != *tc.want {
				t.Fatalf("event = %+v, want %+v", got, tc.want)
			}
		})
	}
}

func TestParseWebhookRespectsConfiguredEvents(t *testing.T) {
	cfg := testConfig("https://gitlab.example.com")
	cfg.Events = []string{"merge_request"} // push and issue not enabled
	g, err := New(cfg, slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatal(err)
	}
	r := webhookRequest("Push Hook", "s3cret", pushByHuman)
	got, err := g.ParseWebhook(r, []byte(pushByHuman))
	if err != nil || got != nil {
		t.Fatalf("push should be ignored when not configured, got %+v, %v", got, err)
	}
}

func TestDiscoverRepos(t *testing.T) {
	// Two pages of group projects.
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v4/groups/top-group/projects", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("include_subgroups") != "true" {
			t.Errorf("include_subgroups not set: %s", r.URL.RawQuery)
		}
		if r.URL.Query().Get("archived") != "false" {
			t.Errorf("archived filter not set: %s", r.URL.RawQuery)
		}
		w.Header().Set("Content-Type", "application/json")
		page := r.URL.Query().Get("page")
		if page == "" || page == "1" {
			w.Header().Set("X-Next-Page", "2")
			fmt.Fprint(w, `[{"path_with_namespace": "top-group/app-1"}, {"path_with_namespace": "top-group/sub/app-2"}]`)
			return
		}
		w.Header().Set("X-Next-Page", "")
		fmt.Fprint(w, `[{"path_with_namespace": "top-group/app-3"}]`)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	g := newTestPlatform(t, srv.URL)
	repos, err := g.DiscoverRepos(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	want := []platform.Repo{
		{Platform: "gl", FullName: "top-group/app-1"},
		{Platform: "gl", FullName: "top-group/sub/app-2"},
		{Platform: "gl", FullName: "top-group/app-3"},
	}
	if len(repos) != len(want) {
		t.Fatalf("repos = %v, want %v", repos, want)
	}
	for i := range want {
		if repos[i] != want[i] {
			t.Errorf("repos[%d] = %v, want %v", i, repos[i], want[i])
		}
	}
	_ = json.Valid // keep import if unused after edits
}
```

(Remove the trailing `json.Valid` line and the `encoding/json` import if unused.)

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/platform/gitlab/ -v`
Expected: FAIL — `undefined: New`.

- [ ] **Step 3: Implement `internal/platform/gitlab/gitlab.go`**

```go
// Package gitlab adapts a GitLab instance to the platform interface:
// group webhook parsing and project discovery.
package gitlab

import (
	"context"
	"crypto/subtle"
	"fmt"
	"log/slog"
	"net/http"
	"strings"

	gogitlab "gitlab.com/gitlab-org/api/client-go"

	"github.com/BlackDark/renovate-server/internal/config"
	"github.com/BlackDark/renovate-server/internal/platform"
)

type GitLab struct {
	name            string
	client          *gogitlab.Client
	webhookPath     string
	secret          string
	botEmail        string
	events          map[string]bool
	groups          []string
	excludeArchived bool
	schedule        config.Schedule
	log             *slog.Logger
}

func New(cfg config.Platform, log *slog.Logger) (*GitLab, error) {
	client, err := gogitlab.NewClient(cfg.Token, gogitlab.WithBaseURL(cfg.BaseURL))
	if err != nil {
		return nil, fmt.Errorf("create gitlab client: %w", err)
	}
	events := make(map[string]bool, len(cfg.Events))
	for _, e := range cfg.Events {
		events[e] = true
	}
	return &GitLab{
		name:            cfg.Name,
		client:          client,
		webhookPath:     cfg.Webhook.Path,
		secret:          cfg.Webhook.Secret,
		botEmail:        cfg.BotEmail,
		events:          events,
		groups:          cfg.Discovery.Groups,
		excludeArchived: cfg.Discovery.ExcludeArchived,
		schedule:        cfg.Schedule,
		log:             log.With("platform", cfg.Name),
	}, nil
}

func (g *GitLab) Name() string              { return g.name }
func (g *GitLab) WebhookPath() string       { return g.webhookPath }
func (g *GitLab) Schedule() config.Schedule { return g.schedule }

// Client exposes the authenticated API client for the gitlabci executor.
func (g *GitLab) Client() *gogitlab.Client { return g.client }

func (g *GitLab) ParseWebhook(r *http.Request, body []byte) (*platform.Event, error) {
	token := r.Header.Get("X-Gitlab-Token")
	if subtle.ConstantTimeCompare([]byte(token), []byte(g.secret)) != 1 {
		return nil, platform.ErrUnauthorized
	}

	hook, err := gogitlab.ParseWebhook(gogitlab.WebhookEventType(r), body)
	if err != nil {
		return nil, fmt.Errorf("parse gitlab webhook: %w", err)
	}

	switch ev := hook.(type) {
	case *gogitlab.MergeEvent:
		if !g.events["merge_request"] {
			return nil, nil
		}
		if !checkboxTicked(ev.Changes.Description.Previous, ev.Changes.Description.Current) {
			return nil, nil
		}
		return g.event(ev.Project.PathWithNamespace, platform.ReasonMergeRequest), nil

	case *gogitlab.IssueEvent:
		if !g.events["issue"] {
			return nil, nil
		}
		if !checkboxTicked(ev.Changes.Description.Previous, ev.Changes.Description.Current) {
			return nil, nil
		}
		return g.event(ev.Project.PathWithNamespace, platform.ReasonIssue), nil

	case *gogitlab.PushEvent:
		if !g.events["push"] {
			return nil, nil
		}
		if g.botEmail != "" && ev.UserEmail == g.botEmail {
			return nil, nil
		}
		if ev.Ref != "refs/heads/"+ev.Project.DefaultBranch {
			return nil, nil
		}
		return g.event(ev.Project.PathWithNamespace, platform.ReasonPush), nil

	default:
		return nil, nil
	}
}

// checkboxTicked reports whether the number of checked todo items increased
// between the previous and current description.
func checkboxTicked(previous, current string) bool {
	if current == "" {
		return false
	}
	return platform.CheckedItems(current) > platform.CheckedItems(previous)
}

func (g *GitLab) event(fullName string, reason platform.Reason) *platform.Event {
	return &platform.Event{
		Repo:   platform.Repo{Platform: g.name, FullName: fullName},
		Reason: reason,
	}
}

func (g *GitLab) DiscoverRepos(ctx context.Context) ([]platform.Repo, error) {
	var repos []platform.Repo
	for _, group := range g.groups {
		opt := &gogitlab.ListGroupProjectsOptions{
			ListOptions:      gogitlab.ListOptions{PerPage: 100},
			IncludeSubGroups: gogitlab.Ptr(true),
		}
		if g.excludeArchived {
			opt.Archived = gogitlab.Ptr(false)
		}
		for {
			projects, resp, err := g.client.Groups.ListGroupProjects(group, opt, gogitlab.WithContext(ctx))
			if err != nil {
				return nil, fmt.Errorf("list projects of group %q: %w", group, err)
			}
			for _, p := range projects {
				name := strings.TrimSpace(p.PathWithNamespace)
				if name == "" {
					continue
				}
				repos = append(repos, platform.Repo{Platform: g.name, FullName: name})
			}
			if resp.NextPage == 0 {
				break
			}
			opt.Page = resp.NextPage
		}
	}
	return repos, nil
}
```

Implementation notes:
- `gogitlab.WithBaseURL` appends `/api/v4` itself when missing — pass `cfg.BaseURL` as-is.
- If the client library's `MergeEvent`/`IssueEvent` `Changes.Description` field shape differs from `struct{ Previous, Current string }` in v1.46.0, adapt the accessor — the tests define behavior via raw JSON payloads, so they stay valid.

- [ ] **Step 4: Add dependency and run tests**

```bash
go get gitlab.com/gitlab-org/api/client-go@v1.46.0
go test -race ./internal/platform/gitlab/
```
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/platform/gitlab go.mod go.sum
git commit -m "feat: add gitlab platform adapter with webhook parsing and discovery"
```

---

### Task 8: GitHub platform adapter

**Files:**
- Create: `internal/platform/github/github.go`
- Test: `internal/platform/github/github_test.go`

**Interfaces:**
- Consumes: `config.Platform`, `platform.Platform` interface, `platform.CheckedItems`.
- Produces: `github.New(cfg config.Platform, log *slog.Logger) (*GitHub, error)` implementing `platform.Platform`.

**Event semantics (mirror of GitLab):**
- `issues` event with `action: edited`: checked count increased from `changes.body.from` to `issue.body`.
- `pull_request` event with `action: edited`: same on `pull_request.body`.
- `push` event: ref is default branch, pusher email differs from `botEmail`.
- Webhook auth: HMAC-SHA256 signature (`X-Hub-Signature-256`).

- [ ] **Step 1: Write the failing test `internal/platform/github/github_test.go`**

```go
package github

import (
	"bytes"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/BlackDark/renovate-server/internal/config"
	"github.com/BlackDark/renovate-server/internal/platform"
)

func testConfig(baseURL string) config.Platform {
	return config.Platform{
		Name:     "gh",
		Type:     config.PlatformGitHub,
		BaseURL:  baseURL,
		Token:    "ghp_test",
		BotEmail: "renovate@example.com",
		Webhook:  config.Webhook{Path: "/webhook/github", Secret: "s3cret"},
		Events:   []string{"merge_request", "issue", "push"},
		Discovery: config.Discovery{Groups: []string{"my-org"}},
	}
}

func newTestPlatform(t *testing.T, baseURL string) *GitHub {
	t.Helper()
	g, err := New(testConfig(baseURL), slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatal(err)
	}
	return g
}

func sign(secret string, body []byte) string {
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(body)
	return "sha256=" + hex.EncodeToString(mac.Sum(nil))
}

func webhookRequest(eventType, secret string, body string) *http.Request {
	r := httptest.NewRequest(http.MethodPost, "/webhook/github", bytes.NewBufferString(body))
	r.Header.Set("X-GitHub-Event", eventType)
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("X-Hub-Signature-256", sign(secret, []byte(body)))
	return r
}

const prTicked = `{
  "action": "edited",
  "pull_request": {"body": "- [x] rebase"},
  "changes": {"body": {"from": "- [ ] rebase"}},
  "repository": {"full_name": "my-org/app", "default_branch": "main"}
}`

const prUnticked = `{
  "action": "edited",
  "pull_request": {"body": "- [ ] rebase"},
  "changes": {"body": {"from": "- [x] rebase"}},
  "repository": {"full_name": "my-org/app", "default_branch": "main"}
}`

const issueTicked = `{
  "action": "edited",
  "issue": {"body": "- [x] approve all"},
  "changes": {"body": {"from": "- [ ] approve all"}},
  "repository": {"full_name": "my-org/app", "default_branch": "main"}
}`

const pushByHuman = `{
  "ref": "refs/heads/main",
  "pusher": {"name": "dev", "email": "dev@example.com"},
  "repository": {"full_name": "my-org/app", "default_branch": "main"}
}`

const pushByBot = `{
  "ref": "refs/heads/main",
  "pusher": {"name": "renovate", "email": "renovate@example.com"},
  "repository": {"full_name": "my-org/app", "default_branch": "main"}
}`

func TestParseWebhookAuth(t *testing.T) {
	g := newTestPlatform(t, "")
	r := webhookRequest("pull_request", "wrong", prTicked)
	_, err := g.ParseWebhook(r, []byte(prTicked))
	if !errors.Is(err, platform.ErrUnauthorized) {
		t.Fatalf("want ErrUnauthorized, got %v", err)
	}
}

func TestParseWebhookEvents(t *testing.T) {
	g := newTestPlatform(t, "")
	cases := []struct {
		name      string
		eventType string
		body      string
		want      *platform.Event
	}{
		{"pr checkbox ticked", "pull_request", prTicked, &platform.Event{
			Repo:   platform.Repo{Platform: "gh", FullName: "my-org/app"},
			Reason: platform.ReasonMergeRequest,
		}},
		{"pr checkbox unticked", "pull_request", prUnticked, nil},
		{"issue checkbox ticked", "issues", issueTicked, &platform.Event{
			Repo:   platform.Repo{Platform: "gh", FullName: "my-org/app"},
			Reason: platform.ReasonIssue,
		}},
		{"push by human", "push", pushByHuman, &platform.Event{
			Repo:   platform.Repo{Platform: "gh", FullName: "my-org/app"},
			Reason: platform.ReasonPush,
		}},
		{"push by bot ignored", "push", pushByBot, nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := webhookRequest(tc.eventType, "s3cret", tc.body)
			got, err := g.ParseWebhook(r, []byte(tc.body))
			if err != nil {
				t.Fatalf("ParseWebhook: %v", err)
			}
			if tc.want == nil {
				if got != nil {
					t.Fatalf("want no event, got %+v", got)
				}
				return
			}
			if got == nil || *got != *tc.want {
				t.Fatalf("event = %+v, want %+v", got, tc.want)
			}
		})
	}
}

func TestDiscoverRepos(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v3/orgs/my-org/repos", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Query().Get("page") == "2" {
			fmt.Fprint(w, `[{"full_name": "my-org/app-3"}]`)
			return
		}
		w.Header().Set("Link", fmt.Sprintf(`<http://%s/api/v3/orgs/my-org/repos?page=2>; rel="next"`, r.Host))
		fmt.Fprint(w, `[{"full_name": "my-org/app-1"}, {"full_name": "my-org/app-2", "archived": true}]`)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	cfg := testConfig(srv.URL)
	cfg.Discovery.ExcludeArchived = true
	g, err := New(cfg, slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatal(err)
	}
	repos, err := g.DiscoverRepos(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	want := []platform.Repo{
		{Platform: "gh", FullName: "my-org/app-1"},
		{Platform: "gh", FullName: "my-org/app-3"},
	}
	if len(repos) != len(want) {
		t.Fatalf("repos = %v, want %v", repos, want)
	}
	for i := range want {
		if repos[i] != want[i] {
			t.Errorf("repos[%d] = %v, want %v", i, repos[i], want[i])
		}
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/platform/github/ -v`
Expected: FAIL — `undefined: New`.

- [ ] **Step 3: Implement `internal/platform/github/github.go`**

```go
// Package github adapts GitHub (cloud or enterprise) to the platform
// interface: org webhook parsing and repo discovery.
package github

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"

	gogithub "github.com/google/go-github/v76/github"

	"github.com/BlackDark/renovate-server/internal/config"
	"github.com/BlackDark/renovate-server/internal/platform"
)

type GitHub struct {
	name            string
	client          *gogithub.Client
	webhookPath     string
	secret          []byte
	botEmail        string
	events          map[string]bool
	orgs            []string
	excludeArchived bool
	schedule        config.Schedule
	log             *slog.Logger
}

func New(cfg config.Platform, log *slog.Logger) (*GitHub, error) {
	client := gogithub.NewClient(nil).WithAuthToken(cfg.Token)
	if cfg.BaseURL != "" && cfg.BaseURL != "https://github.com" {
		var err error
		client, err = client.WithEnterpriseURLs(cfg.BaseURL, cfg.BaseURL)
		if err != nil {
			return nil, fmt.Errorf("create github enterprise client: %w", err)
		}
	}
	events := make(map[string]bool, len(cfg.Events))
	for _, e := range cfg.Events {
		events[e] = true
	}
	return &GitHub{
		name:            cfg.Name,
		client:          client,
		webhookPath:     cfg.Webhook.Path,
		secret:          []byte(cfg.Webhook.Secret),
		botEmail:        cfg.BotEmail,
		events:          events,
		orgs:            cfg.Discovery.Groups,
		excludeArchived: cfg.Discovery.ExcludeArchived,
		schedule:        cfg.Schedule,
		log:             log.With("platform", cfg.Name),
	}, nil
}

func (g *GitHub) Name() string              { return g.name }
func (g *GitHub) WebhookPath() string       { return g.webhookPath }
func (g *GitHub) Schedule() config.Schedule { return g.schedule }

func (g *GitHub) ParseWebhook(r *http.Request, body []byte) (*platform.Event, error) {
	sig := r.Header.Get(gogithub.SHA256SignatureHeader)
	if err := gogithub.ValidateSignature(sig, body, g.secret); err != nil {
		return nil, platform.ErrUnauthorized
	}

	hook, err := gogithub.ParseWebHook(gogithub.WebHookType(r), body)
	if err != nil {
		return nil, fmt.Errorf("parse github webhook: %w", err)
	}

	switch ev := hook.(type) {
	case *gogithub.PullRequestEvent:
		if !g.events["merge_request"] || ev.GetAction() != "edited" {
			return nil, nil
		}
		if !checkboxTicked(previousBody(ev.GetChanges()), ev.GetPullRequest().GetBody()) {
			return nil, nil
		}
		return g.event(ev.GetRepo().GetFullName(), platform.ReasonMergeRequest), nil

	case *gogithub.IssuesEvent:
		if !g.events["issue"] || ev.GetAction() != "edited" {
			return nil, nil
		}
		if !checkboxTicked(previousBody(ev.GetChanges()), ev.GetIssue().GetBody()) {
			return nil, nil
		}
		return g.event(ev.GetRepo().GetFullName(), platform.ReasonIssue), nil

	case *gogithub.PushEvent:
		if !g.events["push"] {
			return nil, nil
		}
		if g.botEmail != "" && ev.GetPusher().GetEmail() == g.botEmail {
			return nil, nil
		}
		if ev.GetRef() != "refs/heads/"+ev.GetRepo().GetDefaultBranch() {
			return nil, nil
		}
		return g.event(ev.GetRepo().GetFullName(), platform.ReasonPush), nil

	default:
		return nil, nil
	}
}

func previousBody(changes *gogithub.EditChange) string {
	if changes == nil || changes.Body == nil || changes.Body.From == nil {
		return ""
	}
	return *changes.Body.From
}

func checkboxTicked(previous, current string) bool {
	if current == "" {
		return false
	}
	return platform.CheckedItems(current) > platform.CheckedItems(previous)
}

func (g *GitHub) event(fullName string, reason platform.Reason) *platform.Event {
	return &platform.Event{
		Repo:   platform.Repo{Platform: g.name, FullName: fullName},
		Reason: reason,
	}
}

func (g *GitHub) DiscoverRepos(ctx context.Context) ([]platform.Repo, error) {
	var repos []platform.Repo
	for _, org := range g.orgs {
		opt := &gogithub.RepositoryListByOrgOptions{
			ListOptions: gogithub.ListOptions{PerPage: 100},
		}
		for {
			page, resp, err := g.client.Repositories.ListByOrg(ctx, org, opt)
			if err != nil {
				return nil, fmt.Errorf("list repos of org %q: %w", org, err)
			}
			for _, repo := range page {
				if g.excludeArchived && repo.GetArchived() {
					continue
				}
				repos = append(repos, platform.Repo{Platform: g.name, FullName: repo.GetFullName()})
			}
			if resp.NextPage == 0 {
				break
			}
			opt.Page = resp.NextPage
		}
	}
	return repos, nil
}
```

Implementation notes:
- go-github's `PushEvent.Repo` is `*PushEventRepository` — `GetFullName()`/`GetDefaultBranch()` exist on it. `PullRequestEvent`/`IssuesEvent` use `*Repository`. Both expose the same getters, so the code above compiles for both.
- `WithEnterpriseURLs` appends `/api/v3/` to the base URL — that's why the test mux serves `/api/v3/orgs/...`.

- [ ] **Step 4: Add dependency and run tests**

```bash
go get github.com/google/go-github/v76@v76.0.0
go test -race ./internal/platform/github/
```
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/platform/github go.mod go.sum
git commit -m "feat: add github platform adapter"
```

---

### Task 9: GitLab pipeline executor

**Files:**
- Create: `internal/executor/gitlabci/gitlabci.go`
- Test: `internal/executor/gitlabci/gitlabci_test.go`

**Interfaces:**
- Consumes: `config.Executor`, `*gogitlab.Client` (from `gitlab.GitLab.Client()`), `executor.RunSpec`.
- Produces: `gitlabci.New(cfg config.Executor, client *gogitlab.Client, log *slog.Logger) (*Executor, error)` implementing `executor.Executor`.

**Behavior:** render `variables` values as Go templates with data `{Repo, Platform, Reason}`, POST the pipeline trigger, then poll `GET /projects/:id/pipelines/:pid` every `pollInterval` until terminal status. `success` → nil; `failed|canceled|skipped` → error; ctx cancel → ctx.Err(). Up to 5 consecutive poll errors tolerated (transient API hiccups), then fail.

- [ ] **Step 1: Write the failing test `internal/executor/gitlabci/gitlabci_test.go`**

```go
package gitlabci

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	gogitlab "gitlab.com/gitlab-org/api/client-go"

	"github.com/BlackDark/renovate-server/internal/config"
	"github.com/BlackDark/renovate-server/internal/executor"
	"github.com/BlackDark/renovate-server/internal/platform"
)

type pipelineServer struct {
	*httptest.Server
	triggered   atomic.Int32
	polls       atomic.Int32
	finalStatus string
	pollsUntilFinal int32
	gotVars     map[string]string
	gotRef      string
}

func newPipelineServer(t *testing.T, finalStatus string, pollsUntilFinal int32) *pipelineServer {
	t.Helper()
	ps := &pipelineServer{finalStatus: finalStatus, pollsUntilFinal: pollsUntilFinal}
	mux := http.NewServeMux()
	mux.HandleFunc("POST /api/v4/projects/infra%2Frenovate-runner/trigger/pipeline", func(w http.ResponseWriter, r *http.Request) {
		ps.triggered.Add(1)
		if err := r.ParseForm(); err != nil {
			t.Error(err)
		}
		ps.gotRef = r.FormValue("ref")
		ps.gotVars = map[string]string{}
		for k, v := range r.Form {
			if len(k) > 11 && k[:10] == "variables[" {
				ps.gotVars[k[10:len(k)-1]] = v[0]
			}
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		fmt.Fprint(w, `{"id": 42, "status": "created"}`)
	})
	mux.HandleFunc("GET /api/v4/projects/infra%2Frenovate-runner/pipelines/42", func(w http.ResponseWriter, r *http.Request) {
		n := ps.polls.Add(1)
		status := "running"
		if n >= ps.pollsUntilFinal {
			status = ps.finalStatus
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{"id": 42, "status": status})
	})
	ps.Server = httptest.NewServer(mux)
	t.Cleanup(ps.Close)
	return ps
}

func newExecutor(t *testing.T, baseURL string) *Executor {
	t.Helper()
	client, err := gogitlab.NewClient("tok", gogitlab.WithBaseURL(baseURL))
	if err != nil {
		t.Fatal(err)
	}
	e, err := New(config.Executor{
		Name:         "ci",
		Type:         config.ExecutorGitLabPipeline,
		Project:      "infra/renovate-runner",
		Ref:          "main",
		TriggerToken: "trigger-tok",
		Variables: map[string]string{
			"RENOVATE_REPO":     "{{ .Repo }}",
			"TRIGGER_REASON":    "{{ .Reason }}",
			"STATIC_VAR":        "fixed",
		},
		PollInterval: 5 * time.Millisecond,
	}, client, slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatal(err)
	}
	return e
}

func spec() executor.RunSpec {
	return executor.RunSpec{
		Repo:   platform.Repo{Platform: "gl", FullName: "top-group/app"},
		Reason: platform.ReasonMergeRequest,
	}
}

func TestRunSuccess(t *testing.T) {
	srv := newPipelineServer(t, "success", 3)
	e := newExecutor(t, srv.URL)
	if err := e.Run(t.Context(), spec()); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if srv.triggered.Load() != 1 {
		t.Errorf("triggered %d times", srv.triggered.Load())
	}
	if srv.gotRef != "main" {
		t.Errorf("ref = %q", srv.gotRef)
	}
	if srv.gotVars["RENOVATE_REPO"] != "top-group/app" {
		t.Errorf("RENOVATE_REPO = %q, want top-group/app", srv.gotVars["RENOVATE_REPO"])
	}
	if srv.gotVars["TRIGGER_REASON"] != "merge_request" {
		t.Errorf("TRIGGER_REASON = %q", srv.gotVars["TRIGGER_REASON"])
	}
	if srv.gotVars["STATIC_VAR"] != "fixed" {
		t.Errorf("STATIC_VAR = %q", srv.gotVars["STATIC_VAR"])
	}
}

func TestRunPipelineFailed(t *testing.T) {
	srv := newPipelineServer(t, "failed", 2)
	e := newExecutor(t, srv.URL)
	err := e.Run(t.Context(), spec())
	if err == nil {
		t.Fatal("want error for failed pipeline")
	}
}

func TestRunContextCancelled(t *testing.T) {
	srv := newPipelineServer(t, "success", 1000) // stays running
	e := newExecutor(t, srv.URL)
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Millisecond)
	defer cancel()
	err := e.Run(ctx, spec())
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("want DeadlineExceeded, got %v", err)
	}
}

func TestInvalidTemplateRejectedAtConstruction(t *testing.T) {
	client, _ := gogitlab.NewClient("tok")
	_, err := New(config.Executor{
		Name: "ci", Project: "p", TriggerToken: "t", Ref: "main",
		Variables: map[string]string{"BAD": "{{ .Nope"},
		PollInterval: time.Second,
	}, client, slog.New(slog.DiscardHandler))
	if err == nil {
		t.Fatal("want template parse error at construction")
	}
}
```

Note: the trigger endpoint path uses the URL-encoded project id (`infra%2Frenovate-runner`). Go's `http.ServeMux` matches on the decoded path — if the pattern with `%2F` does not match, register `mux.HandleFunc("/api/v4/", ...)` and switch on `r.URL.EscapedPath()` + method instead. The assertions stay identical.

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/executor/gitlabci/ -v`
Expected: FAIL — `undefined: New`.

- [ ] **Step 3: Implement `internal/executor/gitlabci/gitlabci.go`**

```go
// Package gitlabci runs renovate by triggering a pipeline in a central
// GitLab project and polling it to completion.
package gitlabci

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"text/template"
	"time"

	gogitlab "gitlab.com/gitlab-org/api/client-go"

	"github.com/BlackDark/renovate-server/internal/config"
	"github.com/BlackDark/renovate-server/internal/executor"
)

const maxConsecutivePollErrors = 5

type Executor struct {
	name         string
	client       *gogitlab.Client
	project      string
	ref          string
	triggerToken string
	variables    map[string]*template.Template
	pollInterval time.Duration
	log          *slog.Logger
}

// templateData is the render context for variable templates.
type templateData struct {
	Repo     string
	Platform string
	Reason   string
}

func New(cfg config.Executor, client *gogitlab.Client, log *slog.Logger) (*Executor, error) {
	vars := make(map[string]*template.Template, len(cfg.Variables))
	for k, v := range cfg.Variables {
		tmpl, err := template.New(k).Option("missingkey=error").Parse(v)
		if err != nil {
			return nil, fmt.Errorf("executor %q: variable %q: %w", cfg.Name, k, err)
		}
		vars[k] = tmpl
	}
	return &Executor{
		name:         cfg.Name,
		client:       client,
		project:      cfg.Project,
		ref:          cfg.Ref,
		triggerToken: cfg.TriggerToken,
		variables:    vars,
		pollInterval: cfg.PollInterval,
		log:          log.With("executor", cfg.Name),
	}, nil
}

func (e *Executor) Name() string { return e.name }

func (e *Executor) Run(ctx context.Context, spec executor.RunSpec) error {
	data := templateData{
		Repo:     spec.Repo.FullName,
		Platform: spec.Repo.Platform,
		Reason:   string(spec.Reason),
	}
	vars := make(map[string]string, len(e.variables))
	for k, tmpl := range e.variables {
		var sb strings.Builder
		if err := tmpl.Execute(&sb, data); err != nil {
			return fmt.Errorf("render variable %q: %w", k, err)
		}
		vars[k] = sb.String()
	}

	pipeline, _, err := e.client.PipelineTriggers.RunPipelineTrigger(e.project,
		&gogitlab.RunPipelineTriggerOptions{
			Ref:       gogitlab.Ptr(e.ref),
			Token:     gogitlab.Ptr(e.triggerToken),
			Variables: vars,
		}, gogitlab.WithContext(ctx))
	if err != nil {
		return fmt.Errorf("trigger pipeline in %q: %w", e.project, err)
	}
	log := e.log.With("repo", spec.Repo.Key(), "pipeline", pipeline.ID)
	log.Info("pipeline triggered")

	ticker := time.NewTicker(e.pollInterval)
	defer ticker.Stop()
	pollErrors := 0
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
		}

		p, _, err := e.client.Pipelines.GetPipeline(e.project, pipeline.ID, gogitlab.WithContext(ctx))
		if err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			pollErrors++
			if pollErrors >= maxConsecutivePollErrors {
				return fmt.Errorf("poll pipeline %d: %w", pipeline.ID, err)
			}
			log.Warn("pipeline poll failed, retrying", "error", err, "attempt", pollErrors)
			continue
		}
		pollErrors = 0

		switch p.Status {
		case "success":
			return nil
		case "failed", "canceled", "skipped":
			return fmt.Errorf("pipeline %d finished with status %q", pipeline.ID, p.Status)
		default:
			// created|waiting_for_resource|preparing|pending|running|manual|scheduled: keep polling
		}
	}
}
```

- [ ] **Step 4: Run tests**

Run: `go test -race ./internal/executor/gitlabci/`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/executor/gitlabci
git commit -m "feat: add gitlab pipeline trigger executor"
```

---

### Task 10: Kubernetes executor

**Files:**
- Create: `internal/executor/kubernetes/kubernetes.go`
- Test: `internal/executor/kubernetes/kubernetes_test.go`

**Interfaces:**
- Consumes: `config.Executor`, `kubernetes.Interface` (real or fake clientset), `executor.RunSpec`.
- Produces: `kubernetes.New(cfg config.Executor, client k8s.Interface, log *slog.Logger) *Executor` implementing `executor.Executor` AND `executor.Adoptable`. Exported for main: `kubernetes.NewClientFromEnv() (k8s.Interface, error)` (in-cluster config, falls back to `$KUBECONFIG`/`~/.kube/config`).

**Job shape:**
- Name: `renovate-<sha256(repoKey)[:16]>-<unixnano base36>` (≤ 63 chars).
- Labels: `app.kubernetes.io/managed-by: renovate-server`, `renovate-server.io/repo-hash: <sha256(repoKey)[:16]>`.
- Annotations: `renovate-server.io/repo`, `renovate-server.io/platform`, `renovate-server.io/reason`.
- Spec: `backoffLimit: 0`, `ttlSecondsAfterFinished: cfg.JobTTL`, restartPolicy Never, container `renovate` with `cfg.Image`, env `RENOVATE_REPOSITORIES=<fullname>` + `cfg.Env`, securityContext runAsNonRoot + no privilege escalation. If `cfg.CachePVC` set: volume `cache` from that PVC mounted at `/tmp/renovate/cache`.
- Completion: poll job every second (internal const `pollInterval = time.Second`; tests can't tick faster reliably with fake clientset watches, polling keeps it simple): `status.succeeded > 0` → success; `status.failed > 0` → failure.

- [ ] **Step 1: Write the failing test `internal/executor/kubernetes/kubernetes_test.go`**

```go
package kubernetes

import (
	"context"
	"errors"
	"log/slog"
	"testing"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"

	"github.com/BlackDark/renovate-server/internal/config"
	"github.com/BlackDark/renovate-server/internal/executor"
	"github.com/BlackDark/renovate-server/internal/platform"
)

func testExecutor(client *fake.Clientset) *Executor {
	e := New(config.Executor{
		Name:      "k8s",
		Type:      config.ExecutorKubernetes,
		Namespace: "renovate",
		Image:     "renovate/renovate:41",
		CachePVC:  "renovate-cache",
		JobTTL:    time.Hour,
		Env:       map[string]string{"RENOVATE_REDIS_URL": "redis://cache:6379"},
	}, client, slog.New(slog.DiscardHandler))
	e.pollInterval = 5 * time.Millisecond // speed up tests
	return e
}

func spec() executor.RunSpec {
	return executor.RunSpec{
		Repo:   platform.Repo{Platform: "gl", FullName: "top-group/app"},
		Reason: platform.ReasonCron,
	}
}

// completeJob polls until a job exists, then marks it succeeded/failed.
func completeJob(t *testing.T, client *fake.Clientset, succeed bool) {
	t.Helper()
	go func() {
		deadline := time.Now().Add(5 * time.Second)
		for time.Now().Before(deadline) {
			jobs, err := client.BatchV1().Jobs("renovate").List(context.Background(), metav1.ListOptions{})
			if err == nil && len(jobs.Items) > 0 {
				job := jobs.Items[0]
				if succeed {
					job.Status.Succeeded = 1
				} else {
					job.Status.Failed = 1
				}
				_, _ = client.BatchV1().Jobs("renovate").UpdateStatus(context.Background(), &job, metav1.UpdateOptions{})
				return
			}
			time.Sleep(5 * time.Millisecond)
		}
	}()
}

func TestRunCreatesJobAndSucceeds(t *testing.T) {
	client := fake.NewClientset()
	e := testExecutor(client)
	completeJob(t, client, true)
	if err := e.Run(t.Context(), spec()); err != nil {
		t.Fatalf("Run: %v", err)
	}

	jobs, _ := client.BatchV1().Jobs("renovate").List(t.Context(), metav1.ListOptions{})
	if len(jobs.Items) != 1 {
		t.Fatalf("jobs = %d, want 1", len(jobs.Items))
	}
	job := jobs.Items[0]
	if job.Labels["app.kubernetes.io/managed-by"] != "renovate-server" {
		t.Errorf("managed-by label missing: %v", job.Labels)
	}
	if job.Annotations["renovate-server.io/repo"] != "top-group/app" {
		t.Errorf("repo annotation = %q", job.Annotations["renovate-server.io/repo"])
	}
	if job.Annotations["renovate-server.io/platform"] != "gl" {
		t.Errorf("platform annotation = %q", job.Annotations["renovate-server.io/platform"])
	}
	pod := job.Spec.Template.Spec
	if pod.Containers[0].Image != "renovate/renovate:41" {
		t.Errorf("image = %q", pod.Containers[0].Image)
	}
	envMap := map[string]string{}
	for _, ev := range pod.Containers[0].Env {
		envMap[ev.Name] = ev.Value
	}
	if envMap["RENOVATE_REPOSITORIES"] != "top-group/app" {
		t.Errorf("RENOVATE_REPOSITORIES = %q", envMap["RENOVATE_REPOSITORIES"])
	}
	if envMap["RENOVATE_REDIS_URL"] != "redis://cache:6379" {
		t.Errorf("custom env missing: %v", envMap)
	}
	if len(pod.Volumes) != 1 || pod.Volumes[0].PersistentVolumeClaim.ClaimName != "renovate-cache" {
		t.Errorf("cache volume wrong: %+v", pod.Volumes)
	}
	if pod.Containers[0].VolumeMounts[0].MountPath != "/tmp/renovate/cache" {
		t.Errorf("mount = %+v", pod.Containers[0].VolumeMounts)
	}
	if *job.Spec.BackoffLimit != 0 {
		t.Errorf("backoffLimit = %d", *job.Spec.BackoffLimit)
	}
	if *job.Spec.TTLSecondsAfterFinished != 3600 {
		t.Errorf("ttl = %d", *job.Spec.TTLSecondsAfterFinished)
	}
	if pod.SecurityContext == nil || pod.SecurityContext.RunAsNonRoot == nil || !*pod.SecurityContext.RunAsNonRoot {
		t.Error("runAsNonRoot not set")
	}
}

func TestRunJobFails(t *testing.T) {
	client := fake.NewClientset()
	e := testExecutor(client)
	completeJob(t, client, false)
	if err := e.Run(t.Context(), spec()); err == nil {
		t.Fatal("want error for failed job")
	}
}

func TestRunContextCancelled(t *testing.T) {
	client := fake.NewClientset()
	e := testExecutor(client)
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Millisecond)
	defer cancel()
	err := e.Run(ctx, spec())
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("want DeadlineExceeded, got %v", err)
	}
}

func TestAdoptRunning(t *testing.T) {
	client := fake.NewClientset()
	// one active adoptable job, one finished job, one foreign job
	active := jobFixture("renovate-abc-1", "top-group/app", "gl")
	finished := jobFixture("renovate-def-2", "top-group/done", "gl")
	finished.Status.Succeeded = 1
	foreign := &batchv1.Job{ObjectMeta: metav1.ObjectMeta{Name: "other", Namespace: "renovate"}}
	for _, j := range []*batchv1.Job{active, finished, foreign} {
		if _, err := client.BatchV1().Jobs("renovate").Create(t.Context(), j, metav1.CreateOptions{}); err != nil {
			t.Fatal(err)
		}
	}

	e := testExecutor(client)
	adopted, err := e.AdoptRunning(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if len(adopted) != 1 {
		t.Fatalf("adopted = %d, want 1", len(adopted))
	}
	if adopted[0].Repo != (platform.Repo{Platform: "gl", FullName: "top-group/app"}) {
		t.Fatalf("adopted repo = %+v", adopted[0].Repo)
	}

	// Wait resolves when the job completes.
	done := make(chan error, 1)
	go func() { done <- adopted[0].Wait(t.Context()) }()
	time.Sleep(10 * time.Millisecond)
	got, _ := client.BatchV1().Jobs("renovate").Get(t.Context(), "renovate-abc-1", metav1.GetOptions{})
	got.Status.Succeeded = 1
	client.BatchV1().Jobs("renovate").UpdateStatus(t.Context(), got, metav1.UpdateOptions{})
	if err := <-done; err != nil {
		t.Fatalf("Wait: %v", err)
	}
}

func jobFixture(name, repo, platformName string) *batchv1.Job {
	return &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: "renovate",
			Labels:    map[string]string{"app.kubernetes.io/managed-by": "renovate-server"},
			Annotations: map[string]string{
				"renovate-server.io/repo":     repo,
				"renovate-server.io/platform": platformName,
				"renovate-server.io/reason":   "push",
			},
		},
		Spec: batchv1.JobSpec{
			Template: corev1.PodTemplateSpec{
				Spec: corev1.PodSpec{
					RestartPolicy: corev1.RestartPolicyNever,
					Containers:    []corev1.Container{{Name: "renovate", Image: "renovate/renovate"}},
				},
			},
		},
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/executor/kubernetes/ -v`
Expected: FAIL — `undefined: New`.

- [ ] **Step 3: Implement `internal/executor/kubernetes/kubernetes.go`**

```go
// Package kubernetes runs renovate as Kubernetes Jobs and can re-adopt
// running Jobs after a server restart via labels.
package kubernetes

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	k8s "k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"

	"github.com/BlackDark/renovate-server/internal/config"
	"github.com/BlackDark/renovate-server/internal/executor"
	"github.com/BlackDark/renovate-server/internal/platform"
)

const (
	labelManagedBy     = "app.kubernetes.io/managed-by"
	labelManagedByVal  = "renovate-server"
	labelRepoHash      = "renovate-server.io/repo-hash"
	annotationRepo     = "renovate-server.io/repo"
	annotationPlatform = "renovate-server.io/platform"
	annotationReason   = "renovate-server.io/reason"
	cacheMountPath     = "/tmp/renovate/cache"
)

type Executor struct {
	name         string
	client       k8s.Interface
	namespace    string
	image        string
	cachePVC     string
	jobTTL       time.Duration
	env          map[string]string
	pollInterval time.Duration
	log          *slog.Logger
}

func New(cfg config.Executor, client k8s.Interface, log *slog.Logger) *Executor {
	return &Executor{
		name:         cfg.Name,
		client:       client,
		namespace:    cfg.Namespace,
		image:        cfg.Image,
		cachePVC:     cfg.CachePVC,
		jobTTL:       cfg.JobTTL,
		env:          cfg.Env,
		pollInterval: time.Second,
		log:          log.With("executor", cfg.Name),
	}
}

// NewClientFromEnv builds a clientset from in-cluster config, falling back
// to $KUBECONFIG / ~/.kube/config for local development.
func NewClientFromEnv() (k8s.Interface, error) {
	cfg, err := rest.InClusterConfig()
	if err != nil {
		path := os.Getenv("KUBECONFIG")
		if path == "" {
			home, herr := os.UserHomeDir()
			if herr != nil {
				return nil, fmt.Errorf("kube config: %w", err)
			}
			path = filepath.Join(home, ".kube", "config")
		}
		cfg, err = clientcmd.BuildConfigFromFlags("", path)
		if err != nil {
			return nil, fmt.Errorf("kube config: %w", err)
		}
	}
	return k8s.NewForConfig(cfg)
}

func (e *Executor) Name() string { return e.name }

func (e *Executor) Run(ctx context.Context, spec executor.RunSpec) error {
	job := e.buildJob(spec)
	created, err := e.client.BatchV1().Jobs(e.namespace).Create(ctx, job, metav1.CreateOptions{})
	if err != nil {
		return fmt.Errorf("create job: %w", err)
	}
	e.log.Info("job created", "job", created.Name, "repo", spec.Repo.Key())
	return e.waitForJob(ctx, created.Name)
}

// AdoptRunning lists managed Jobs still active and returns wait handles so
// the dispatcher can re-lock their repos after a restart.
func (e *Executor) AdoptRunning(ctx context.Context) ([]executor.AdoptedRun, error) {
	list, err := e.client.BatchV1().Jobs(e.namespace).List(ctx, metav1.ListOptions{
		LabelSelector: labelManagedBy + "=" + labelManagedByVal,
	})
	if err != nil {
		return nil, fmt.Errorf("list managed jobs: %w", err)
	}
	var adopted []executor.AdoptedRun
	for i := range list.Items {
		job := list.Items[i]
		if job.Status.Succeeded > 0 || job.Status.Failed > 0 {
			continue
		}
		repo := platform.Repo{
			Platform: job.Annotations[annotationPlatform],
			FullName: job.Annotations[annotationRepo],
		}
		if repo.Platform == "" || repo.FullName == "" {
			continue
		}
		name := job.Name
		adopted = append(adopted, executor.AdoptedRun{
			Repo: repo,
			Wait: func(ctx context.Context) error { return e.waitForJob(ctx, name) },
		})
	}
	sort.Slice(adopted, func(i, j int) bool { return adopted[i].Repo.Key() < adopted[j].Repo.Key() })
	return adopted, nil
}

func (e *Executor) waitForJob(ctx context.Context, name string) error {
	ticker := time.NewTicker(e.pollInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
		}
		job, err := e.client.BatchV1().Jobs(e.namespace).Get(ctx, name, metav1.GetOptions{})
		if err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			return fmt.Errorf("get job %q: %w", name, err)
		}
		if job.Status.Succeeded > 0 {
			return nil
		}
		if job.Status.Failed > 0 {
			return fmt.Errorf("job %q failed", name)
		}
	}
}

func (e *Executor) buildJob(spec executor.RunSpec) *batchv1.Job {
	hash := repoHash(spec.Repo.Key())
	name := fmt.Sprintf("renovate-%s-%s", hash, strconv.FormatInt(time.Now().UnixNano(), 36))

	env := []corev1.EnvVar{{Name: "RENOVATE_REPOSITORIES", Value: spec.Repo.FullName}}
	keys := make([]string, 0, len(e.env))
	for k := range e.env {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		env = append(env, corev1.EnvVar{Name: k, Value: e.env[k]})
	}

	var volumes []corev1.Volume
	var mounts []corev1.VolumeMount
	if e.cachePVC != "" {
		volumes = append(volumes, corev1.Volume{
			Name: "cache",
			VolumeSource: corev1.VolumeSource{
				PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{ClaimName: e.cachePVC},
			},
		})
		mounts = append(mounts, corev1.VolumeMount{Name: "cache", MountPath: cacheMountPath})
	}

	backoffLimit := int32(0)
	ttl := int32(e.jobTTL.Seconds())
	runAsNonRoot := true
	noPrivEsc := false

	return &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: e.namespace,
			Labels: map[string]string{
				labelManagedBy: labelManagedByVal,
				labelRepoHash:  hash,
			},
			Annotations: map[string]string{
				annotationRepo:     spec.Repo.FullName,
				annotationPlatform: spec.Repo.Platform,
				annotationReason:   string(spec.Reason),
			},
		},
		Spec: batchv1.JobSpec{
			BackoffLimit:            &backoffLimit,
			TTLSecondsAfterFinished: &ttl,
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{
					Labels: map[string]string{labelManagedBy: labelManagedByVal},
				},
				Spec: corev1.PodSpec{
					RestartPolicy: corev1.RestartPolicyNever,
					SecurityContext: &corev1.PodSecurityContext{
						RunAsNonRoot: &runAsNonRoot,
					},
					Volumes: volumes,
					Containers: []corev1.Container{{
						Name:         "renovate",
						Image:        e.image,
						Env:          env,
						VolumeMounts: mounts,
						SecurityContext: &corev1.SecurityContext{
							AllowPrivilegeEscalation: &noPrivEsc,
						},
					}},
				},
			},
		},
	}
}

func repoHash(key string) string {
	sum := sha256.Sum256([]byte(key))
	return hex.EncodeToString(sum[:])[:16]
}
```

- [ ] **Step 4: Add dependencies and run tests**

```bash
go get k8s.io/client-go@v0.36.2 k8s.io/api@v0.36.2 k8s.io/apimachinery@v0.36.2
go test -race ./internal/executor/kubernetes/
```
Expected: PASS. (If `fake.NewClientset` is undefined in this client-go version, use `fake.NewSimpleClientset()` — same behavior.)

- [ ] **Step 5: Commit**

```bash
git add internal/executor/kubernetes go.mod go.sum
git commit -m "feat: add kubernetes job executor with restart re-adoption"
```

---

### Task 11: Docker executor

**Files:**
- Create: `internal/executor/docker/docker.go`
- Test: `internal/executor/docker/docker_test.go`

**Interfaces:**
- Consumes: `config.Executor`, `executor.RunSpec`.
- Produces: `docker.New(cfg config.Executor, api API, log *slog.Logger) *Executor` implementing `executor.Executor`, plus `docker.NewAPIFromEnv() (API, error)` wrapping the real Docker client. `API` is a thin interface over the SDK so tests use a hand-written fake:

```go
type API interface {
	ImagePull(ctx context.Context, ref string, opts image.PullOptions) (io.ReadCloser, error)
	ContainerCreate(ctx context.Context, cfg *container.Config, host *container.HostConfig,
		netCfg *network.NetworkingConfig, platform *ocispec.Platform, name string) (container.CreateResponse, error)
	ContainerStart(ctx context.Context, id string, opts container.StartOptions) error
	ContainerWait(ctx context.Context, id string, cond container.WaitCondition) (<-chan container.WaitResponse, <-chan error)
	ContainerRemove(ctx context.Context, id string, opts container.RemoveOptions) error
}
```

**Behavior:** optional `ImagePull` when `cfg.Pull`; create container (env `RENOVATE_REPOSITORIES` + `cfg.Env` sorted; bind `cacheVolume:/tmp/renovate/cache` when set); start; wait for `WaitConditionNotRunning`; exit code 0 → nil, else error; always `ContainerRemove(force)` afterwards; on ctx cancel remove container and return ctx.Err().

- [ ] **Step 1: Write the failing test `internal/executor/docker/docker_test.go`**

```go
package docker

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/image"
	"github.com/docker/docker/api/types/network"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"

	"github.com/BlackDark/renovate-server/internal/config"
	"github.com/BlackDark/renovate-server/internal/executor"
	"github.com/BlackDark/renovate-server/internal/platform"
)

type fakeAPI struct {
	mu         sync.Mutex
	pulled     []string
	created    *container.Config
	hostCfg    *container.HostConfig
	started    bool
	removed    bool
	exitCode   int64
	waitDelay  time.Duration
}

func (f *fakeAPI) ImagePull(_ context.Context, ref string, _ image.PullOptions) (io.ReadCloser, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.pulled = append(f.pulled, ref)
	return io.NopCloser(strings.NewReader("{}")), nil
}

func (f *fakeAPI) ContainerCreate(_ context.Context, cfg *container.Config, host *container.HostConfig,
	_ *network.NetworkingConfig, _ *ocispec.Platform, _ string) (container.CreateResponse, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.created = cfg
	f.hostCfg = host
	return container.CreateResponse{ID: "cid-1"}, nil
}

func (f *fakeAPI) ContainerStart(_ context.Context, id string, _ container.StartOptions) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.started = true
	return nil
}

func (f *fakeAPI) ContainerWait(ctx context.Context, id string, _ container.WaitCondition) (<-chan container.WaitResponse, <-chan error) {
	respCh := make(chan container.WaitResponse, 1)
	errCh := make(chan error, 1)
	go func() {
		select {
		case <-time.After(f.waitDelay):
			f.mu.Lock()
			code := f.exitCode
			f.mu.Unlock()
			respCh <- container.WaitResponse{StatusCode: code}
		case <-ctx.Done():
			errCh <- ctx.Err()
		}
	}()
	return respCh, errCh
}

func (f *fakeAPI) ContainerRemove(_ context.Context, id string, _ container.RemoveOptions) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.removed = true
	return nil
}

func testExecutor(api API, pull bool) *Executor {
	return New(config.Executor{
		Name:        "docker",
		Type:        config.ExecutorDocker,
		Image:       "renovate/renovate:41",
		CacheVolume: "renovate-cache",
		Pull:        pull,
		Env:         map[string]string{"LOG_LEVEL": "debug"},
	}, api, slog.New(slog.DiscardHandler))
}

func spec() executor.RunSpec {
	return executor.RunSpec{
		Repo:   platform.Repo{Platform: "gl", FullName: "top-group/app"},
		Reason: platform.ReasonPush,
	}
}

func TestRunSuccess(t *testing.T) {
	api := &fakeAPI{}
	e := testExecutor(api, false)
	if err := e.Run(t.Context(), spec()); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(api.pulled) != 0 {
		t.Errorf("pull not configured but pulled %v", api.pulled)
	}
	if api.created.Image != "renovate/renovate:41" {
		t.Errorf("image = %q", api.created.Image)
	}
	wantEnv := []string{"RENOVATE_REPOSITORIES=top-group/app", "LOG_LEVEL=debug"}
	for _, w := range wantEnv {
		found := false
		for _, e := range api.created.Env {
			if e == w {
				found = true
			}
		}
		if !found {
			t.Errorf("env missing %q in %v", w, api.created.Env)
		}
	}
	if len(api.hostCfg.Binds) != 1 || api.hostCfg.Binds[0] != "renovate-cache:/tmp/renovate/cache" {
		t.Errorf("binds = %v", api.hostCfg.Binds)
	}
	if !api.started || !api.removed {
		t.Errorf("started=%v removed=%v, want both true", api.started, api.removed)
	}
}

func TestRunPullsWhenConfigured(t *testing.T) {
	api := &fakeAPI{}
	e := testExecutor(api, true)
	if err := e.Run(t.Context(), spec()); err != nil {
		t.Fatal(err)
	}
	if len(api.pulled) != 1 || api.pulled[0] != "renovate/renovate:41" {
		t.Errorf("pulled = %v", api.pulled)
	}
}

func TestRunNonZeroExit(t *testing.T) {
	api := &fakeAPI{exitCode: 2}
	e := testExecutor(api, false)
	err := e.Run(t.Context(), spec())
	if err == nil || !strings.Contains(err.Error(), "exit code 2") {
		t.Fatalf("want exit code error, got %v", err)
	}
	if !api.removed {
		t.Error("container not removed after failure")
	}
}

func TestRunContextCancelled(t *testing.T) {
	api := &fakeAPI{waitDelay: time.Minute}
	e := testExecutor(api, false)
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Millisecond)
	defer cancel()
	err := e.Run(ctx, spec())
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("want DeadlineExceeded, got %v", err)
	}
	if !api.removed {
		t.Error("container not removed after cancellation")
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/executor/docker/ -v`
Expected: FAIL — `undefined: New`, `undefined: API`.

- [ ] **Step 3: Implement `internal/executor/docker/docker.go`**

```go
// Package docker runs renovate as containers against a local Docker daemon.
package docker

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"sort"

	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/image"
	"github.com/docker/docker/api/types/network"
	"github.com/docker/docker/client"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"

	"github.com/BlackDark/renovate-server/internal/config"
	"github.com/BlackDark/renovate-server/internal/executor"
)

const cacheMountPath = "/tmp/renovate/cache"

// API is the subset of the Docker SDK the executor needs; *client.Client
// satisfies it, tests use a fake.
type API interface {
	ImagePull(ctx context.Context, ref string, opts image.PullOptions) (io.ReadCloser, error)
	ContainerCreate(ctx context.Context, cfg *container.Config, host *container.HostConfig,
		netCfg *network.NetworkingConfig, platform *ocispec.Platform, name string) (container.CreateResponse, error)
	ContainerStart(ctx context.Context, id string, opts container.StartOptions) error
	ContainerWait(ctx context.Context, id string, cond container.WaitCondition) (<-chan container.WaitResponse, <-chan error)
	ContainerRemove(ctx context.Context, id string, opts container.RemoveOptions) error
}

// NewAPIFromEnv creates a Docker client from DOCKER_HOST etc.
func NewAPIFromEnv() (API, error) {
	return client.NewClientWithOpts(client.FromEnv, client.WithAPIVersionNegotiation())
}

type Executor struct {
	name        string
	api         API
	image       string
	cacheVolume string
	pull        bool
	env         map[string]string
	log         *slog.Logger
}

func New(cfg config.Executor, api API, log *slog.Logger) *Executor {
	return &Executor{
		name:        cfg.Name,
		api:         api,
		image:       cfg.Image,
		cacheVolume: cfg.CacheVolume,
		pull:        cfg.Pull,
		env:         cfg.Env,
		log:         log.With("executor", cfg.Name),
	}
}

func (e *Executor) Name() string { return e.name }

func (e *Executor) Run(ctx context.Context, spec executor.RunSpec) error {
	if e.pull {
		rc, err := e.api.ImagePull(ctx, e.image, image.PullOptions{})
		if err != nil {
			return fmt.Errorf("pull image %q: %w", e.image, err)
		}
		_, _ = io.Copy(io.Discard, rc)
		_ = rc.Close()
	}

	env := []string{"RENOVATE_REPOSITORIES=" + spec.Repo.FullName}
	keys := make([]string, 0, len(e.env))
	for k := range e.env {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		env = append(env, k+"="+e.env[k])
	}

	hostCfg := &container.HostConfig{}
	if e.cacheVolume != "" {
		hostCfg.Binds = []string{e.cacheVolume + ":" + cacheMountPath}
	}

	created, err := e.api.ContainerCreate(ctx, &container.Config{
		Image: e.image,
		Env:   env,
	}, hostCfg, nil, nil, "")
	if err != nil {
		return fmt.Errorf("create container: %w", err)
	}
	log := e.log.With("repo", spec.Repo.Key(), "container", created.ID)

	// Removal must succeed even when ctx is already cancelled.
	defer func() {
		removeCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30_000_000_000) // 30s
		defer cancel()
		if err := e.api.ContainerRemove(removeCtx, created.ID, container.RemoveOptions{Force: true}); err != nil {
			log.Warn("container remove failed", "error", err)
		}
	}()

	if err := e.api.ContainerStart(ctx, created.ID, container.StartOptions{}); err != nil {
		return fmt.Errorf("start container: %w", err)
	}
	log.Info("container started")

	respCh, errCh := e.api.ContainerWait(ctx, created.ID, container.WaitConditionNotRunning)
	select {
	case resp := <-respCh:
		if resp.StatusCode != 0 {
			return fmt.Errorf("renovate container finished with exit code %d", resp.StatusCode)
		}
		return nil
	case err := <-errCh:
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return fmt.Errorf("wait for container: %w", err)
	case <-ctx.Done():
		return ctx.Err()
	}
}
```

Replace the numeric timeout literal with `30*time.Second` and import `time` — written out here to make the intent explicit.

- [ ] **Step 4: Add dependencies and run tests**

```bash
go get github.com/docker/docker@v28.5.2+incompatible github.com/opencontainers/image-spec@latest
go test -race ./internal/executor/docker/
```
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/executor/docker go.mod go.sum
git commit -m "feat: add docker container executor"
```

---

### Task 12: Metrics + HTTP server

**Files:**
- Create: `internal/metrics/metrics.go`, `internal/server/server.go`
- Test: `internal/server/server_test.go`

**Interfaces:**
- Consumes: `platform.Platform`, `store.Store`, dispatcher's `Enqueue(ev platform.Event)`.
- Produces:

```go
// internal/metrics
func New(reg *prometheus.Registry, st store.Store) *Metrics
// implements dispatch.Metrics:
func (m *Metrics) RunStarted(executorName string)
func (m *Metrics) RunFinished(executorName, result string, seconds float64)
// called by server:
func (m *Metrics) WebhookEvent(platformName, outcome string) // accepted|ignored|unauthorized|invalid

// internal/server
type Enqueuer interface { Enqueue(ev platform.Event) }
func New(platforms []platform.Platform, enq Enqueuer, st store.Store,
	reg *prometheus.Registry, m *metrics.Metrics, log *slog.Logger) *Server
func (s *Server) Handler() http.Handler
func (s *Server) SetReady(ready bool)
```

**Routes:** `POST <platform webhook path>` per platform; `GET /healthz` (always 200), `GET /readyz` (503 until SetReady(true)), `GET /metrics`, `GET /api/v1/status`. Webhook bodies limited to 1 MiB via `http.MaxBytesReader`.

- [ ] **Step 1: Write `internal/metrics/metrics.go` (thin, exercised via server test + dispatcher integration)**

```go
// Package metrics exposes Prometheus instrumentation for the server.
package metrics

import (
	"github.com/prometheus/client_golang/prometheus"

	"github.com/BlackDark/renovate-server/internal/store"
)

type Metrics struct {
	webhookEvents *prometheus.CounterVec
	runsStarted   *prometheus.CounterVec
	runsFinished  *prometheus.CounterVec
	runDuration   *prometheus.HistogramVec
}

// New registers all metrics on reg. The store feeds the repo-state gauge.
func New(reg *prometheus.Registry, st store.Store) *Metrics {
	m := &Metrics{
		webhookEvents: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "renovate_server_webhook_events_total",
			Help: "Webhook events received, by platform and outcome.",
		}, []string{"platform", "outcome"}),
		runsStarted: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "renovate_server_runs_started_total",
			Help: "Renovate runs started, by executor.",
		}, []string{"executor"}),
		runsFinished: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "renovate_server_runs_finished_total",
			Help: "Renovate runs finished, by executor and result.",
		}, []string{"executor", "result"}),
		runDuration: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Name:    "renovate_server_run_duration_seconds",
			Help:    "Duration of renovate runs, by executor.",
			Buckets: prometheus.ExponentialBuckets(10, 2, 10), // 10s .. ~85m
		}, []string{"executor"}),
	}
	reg.MustRegister(m.webhookEvents, m.runsStarted, m.runsFinished, m.runDuration)
	reg.MustRegister(prometheus.NewGaugeFunc(prometheus.GaugeOpts{
		Name: "renovate_server_repos_active",
		Help: "Repos currently queued or running.",
	}, func() float64 { return float64(len(st.Snapshot())) }))
	return m
}

func (m *Metrics) WebhookEvent(platformName, outcome string) {
	m.webhookEvents.WithLabelValues(platformName, outcome).Inc()
}

func (m *Metrics) RunStarted(executorName string) {
	m.runsStarted.WithLabelValues(executorName).Inc()
}

func (m *Metrics) RunFinished(executorName, result string, seconds float64) {
	m.runsFinished.WithLabelValues(executorName, result).Inc()
	m.runDuration.WithLabelValues(executorName).Observe(seconds)
}
```

- [ ] **Step 2: Write the failing test `internal/server/server_test.go`**

```go
package server

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/BlackDark/renovate-server/internal/config"
	"github.com/BlackDark/renovate-server/internal/metrics"
	"github.com/BlackDark/renovate-server/internal/platform"
	"github.com/BlackDark/renovate-server/internal/store"
)

// fakePlatform returns canned ParseWebhook results.
type fakePlatform struct {
	name string
	path string
	ev   *platform.Event
	err  error
}

func (f *fakePlatform) Name() string              { return f.name }
func (f *fakePlatform) WebhookPath() string       { return f.path }
func (f *fakePlatform) Schedule() config.Schedule { return config.Schedule{} }
func (f *fakePlatform) DiscoverRepos(context.Context) ([]platform.Repo, error) {
	return nil, nil
}
func (f *fakePlatform) ParseWebhook(*http.Request, []byte) (*platform.Event, error) {
	return f.ev, f.err
}

type fakeEnqueuer struct {
	mu     sync.Mutex
	events []platform.Event
}

func (f *fakeEnqueuer) Enqueue(ev platform.Event) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.events = append(f.events, ev)
}

func testServer(t *testing.T, p platform.Platform) (*Server, *fakeEnqueuer, store.Store) {
	t.Helper()
	st := store.NewMemory()
	reg := prometheus.NewRegistry()
	m := metrics.New(reg, st)
	enq := &fakeEnqueuer{}
	s := New([]platform.Platform{p}, enq, st, reg, m, slog.New(slog.DiscardHandler))
	return s, enq, st
}

func TestWebhookAccepted(t *testing.T) {
	ev := &platform.Event{
		Repo:   platform.Repo{Platform: "gl", FullName: "g/a"},
		Reason: platform.ReasonPush,
	}
	s, enq, _ := testServer(t, &fakePlatform{name: "gl", path: "/webhook/gitlab", ev: ev})
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, httptest.NewRequest("POST", "/webhook/gitlab", strings.NewReader("{}")))
	if rec.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want 202", rec.Code)
	}
	if len(enq.events) != 1 || enq.events[0] != *ev {
		t.Fatalf("enqueued = %+v", enq.events)
	}
}

func TestWebhookIgnored(t *testing.T) {
	s, enq, _ := testServer(t, &fakePlatform{name: "gl", path: "/webhook/gitlab"})
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, httptest.NewRequest("POST", "/webhook/gitlab", strings.NewReader("{}")))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if len(enq.events) != 0 {
		t.Fatalf("nothing should be enqueued, got %+v", enq.events)
	}
}

func TestWebhookUnauthorized(t *testing.T) {
	s, _, _ := testServer(t, &fakePlatform{name: "gl", path: "/webhook/gitlab", err: platform.ErrUnauthorized})
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, httptest.NewRequest("POST", "/webhook/gitlab", strings.NewReader("{}")))
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", rec.Code)
	}
}

func TestWebhookMalformed(t *testing.T) {
	s, _, _ := testServer(t, &fakePlatform{name: "gl", path: "/webhook/gitlab", err: errFake})
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, httptest.NewRequest("POST", "/webhook/gitlab", strings.NewReader("{}")))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rec.Code)
	}
}

var errFake = &json.SyntaxError{}

func TestHealthAndReady(t *testing.T) {
	s, _, _ := testServer(t, &fakePlatform{name: "gl", path: "/webhook/gitlab"})
	h := s.Handler()

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/healthz", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("healthz = %d", rec.Code)
	}

	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/readyz", nil))
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("readyz before ready = %d, want 503", rec.Code)
	}
	s.SetReady(true)
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/readyz", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("readyz after ready = %d, want 200", rec.Code)
	}
}

func TestStatusEndpoint(t *testing.T) {
	s, _, st := testServer(t, &fakePlatform{name: "gl", path: "/webhook/gitlab"})
	st.Queue("gl:g/a", "push")
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, httptest.NewRequest("GET", "/api/v1/status", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	var body struct {
		Repos map[string]store.RepoStatus `json:"repos"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if body.Repos["gl:g/a"].State != store.StateQueued {
		t.Fatalf("body = %s", rec.Body.String())
	}
}

func TestMetricsEndpoint(t *testing.T) {
	s, _, _ := testServer(t, &fakePlatform{name: "gl", path: "/webhook/gitlab"})
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, httptest.NewRequest("GET", "/metrics", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("metrics = %d", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "renovate_server_repos_active") {
		t.Error("gauge missing from /metrics output")
	}
}
```

- [ ] **Step 3: Run test to verify it fails**

Run: `go test ./internal/server/ -v`
Expected: FAIL — `undefined: New`.

- [ ] **Step 4: Implement `internal/server/server.go`**

```go
// Package server exposes the HTTP surface: webhook receivers per platform
// and operational endpoints (health, readiness, metrics, status).
package server

import (
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"sync/atomic"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"

	"github.com/BlackDark/renovate-server/internal/metrics"
	"github.com/BlackDark/renovate-server/internal/platform"
	"github.com/BlackDark/renovate-server/internal/store"
)

const maxWebhookBody = 1 << 20 // 1 MiB

// Enqueuer is the dispatcher surface the server needs.
type Enqueuer interface {
	Enqueue(ev platform.Event)
}

type Server struct {
	mux   *http.ServeMux
	ready atomic.Bool
	log   *slog.Logger
}

func New(platforms []platform.Platform, enq Enqueuer, st store.Store,
	reg *prometheus.Registry, m *metrics.Metrics, log *slog.Logger) *Server {
	s := &Server{mux: http.NewServeMux(), log: log}

	for _, p := range platforms {
		s.mux.HandleFunc("POST "+p.WebhookPath(), s.webhookHandler(p, enq, m))
	}

	s.mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	s.mux.HandleFunc("GET /readyz", func(w http.ResponseWriter, _ *http.Request) {
		if !s.ready.Load() {
			http.Error(w, "not ready", http.StatusServiceUnavailable)
			return
		}
		w.WriteHeader(http.StatusOK)
	})
	s.mux.Handle("GET /metrics", promhttp.HandlerFor(reg, promhttp.HandlerOpts{}))
	s.mux.HandleFunc("GET /api/v1/status", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"repos": st.Snapshot()})
	})
	return s
}

func (s *Server) Handler() http.Handler { return s.mux }

// SetReady flips the readiness probe; main calls it after startup completes.
func (s *Server) SetReady(ready bool) { s.ready.Store(ready) }

func (s *Server) webhookHandler(p platform.Platform, enq Enqueuer, m *metrics.Metrics) http.HandlerFunc {
	log := s.log.With("platform", p.Name())
	return func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxWebhookBody))
		if err != nil {
			m.WebhookEvent(p.Name(), "invalid")
			http.Error(w, "failed to read body", http.StatusBadRequest)
			return
		}

		ev, err := p.ParseWebhook(r, body)
		switch {
		case errors.Is(err, platform.ErrUnauthorized):
			m.WebhookEvent(p.Name(), "unauthorized")
			log.Warn("webhook authentication failed", "remote", r.RemoteAddr)
			http.Error(w, "unauthorized", http.StatusUnauthorized)
		case err != nil:
			m.WebhookEvent(p.Name(), "invalid")
			log.Warn("webhook payload invalid", "error", err)
			http.Error(w, "invalid payload", http.StatusBadRequest)
		case ev == nil:
			m.WebhookEvent(p.Name(), "ignored")
			w.WriteHeader(http.StatusOK)
		default:
			m.WebhookEvent(p.Name(), "accepted")
			enq.Enqueue(*ev)
			w.WriteHeader(http.StatusAccepted)
		}
	}
}
```

- [ ] **Step 5: Add dependency and run tests**

```bash
go get github.com/prometheus/client_golang@v1.23.2
go test -race ./internal/server/ ./internal/metrics/
```
Expected: PASS.

- [ ] **Step 6: Commit**

```bash
git add internal/metrics internal/server go.mod go.sum
git commit -m "feat: add http server with webhook routing, metrics and status api"
```

---

### Task 13: Cron scheduler + main wiring + example config

**Files:**
- Create: `internal/schedule/schedule.go`, `cmd/renovate-server/main.go`, `examples/config.yaml`
- Test: `internal/schedule/schedule_test.go`

**Interfaces:**
- Consumes: everything built so far.
- Produces:

```go
// internal/schedule
func New(log *slog.Logger) *Runner
// AddPlatform registers all crontabs of a platform; job runs discovery+enqueue.
func (r *Runner) AddPlatform(sched config.Schedule, job func()) error
func (r *Runner) Start()
func (r *Runner) Stop() context.Context // returns cron's drain context
```

- [ ] **Step 1: Write the failing test `internal/schedule/schedule_test.go`**

```go
package schedule

import (
	"log/slog"
	"sync/atomic"
	"testing"
	"time"

	"github.com/BlackDark/renovate-server/internal/config"
)

func TestAddPlatformValidatesTimezone(t *testing.T) {
	r := New(slog.New(slog.DiscardHandler))
	err := r.AddPlatform(config.Schedule{Crontabs: []string{"* * * * *"}, Timezone: "Mars/Olympus"}, func() {})
	if err == nil {
		t.Fatal("want timezone error")
	}
}

func TestAddPlatformValidatesCrontab(t *testing.T) {
	r := New(slog.New(slog.DiscardHandler))
	err := r.AddPlatform(config.Schedule{Crontabs: []string{"bogus"}}, func() {})
	if err == nil {
		t.Fatal("want crontab error")
	}
}

func TestScheduledJobFires(t *testing.T) {
	r := New(slog.New(slog.DiscardHandler))
	var fired atomic.Int32
	// @every is supported by robfig/cron's standard parser via descriptors.
	if err := r.AddPlatform(config.Schedule{Crontabs: []string{"@every 10ms"}}, func() {
		fired.Add(1)
	}); err != nil {
		t.Fatal(err)
	}
	r.Start()
	defer r.Stop()
	deadline := time.Now().Add(5 * time.Second)
	for fired.Load() == 0 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if fired.Load() == 0 {
		t.Fatal("job never fired")
	}
}

func TestEmptyScheduleIsNoop(t *testing.T) {
	r := New(slog.New(slog.DiscardHandler))
	if err := r.AddPlatform(config.Schedule{}, func() {}); err != nil {
		t.Fatal(err)
	}
	r.Start()
	r.Stop()
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/schedule/ -v`
Expected: FAIL — `undefined: New`.

- [ ] **Step 3: Implement `internal/schedule/schedule.go`**

```go
// Package schedule fires periodic full-discovery renovate runs per platform.
package schedule

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/robfig/cron/v3"

	"github.com/BlackDark/renovate-server/internal/config"
)

type Runner struct {
	crons []*cron.Cron
	log   *slog.Logger
}

func New(log *slog.Logger) *Runner {
	return &Runner{log: log}
}

// AddPlatform registers the platform's crontabs; each firing invokes job.
// Overlapping firings are safe: the dispatcher coalesces per repo.
func (r *Runner) AddPlatform(sched config.Schedule, job func()) error {
	if len(sched.Crontabs) == 0 {
		return nil
	}
	loc := time.UTC
	if sched.Timezone != "" {
		var err error
		loc, err = time.LoadLocation(sched.Timezone)
		if err != nil {
			return fmt.Errorf("invalid timezone %q: %w", sched.Timezone, err)
		}
	}
	c := cron.New(cron.WithLocation(loc))
	for _, tab := range sched.Crontabs {
		if _, err := c.AddFunc(tab, job); err != nil {
			return fmt.Errorf("invalid crontab %q: %w", tab, err)
		}
	}
	r.crons = append(r.crons, c)
	return nil
}

func (r *Runner) Start() {
	for _, c := range r.crons {
		c.Start()
	}
}

// Stop halts scheduling; running jobs drain in the background.
func (r *Runner) Stop() context.Context {
	ctx := context.Background()
	for _, c := range r.crons {
		ctx = c.Stop()
	}
	return ctx
}
```

- [ ] **Step 4: Run schedule tests**

Run: `go test -race ./internal/schedule/`
Expected: PASS.

- [ ] **Step 5: Write `cmd/renovate-server/main.go`**

```go
// Command renovate-server coordinates renovate runs across GitLab/GitHub
// repositories, triggered by webhooks and cron schedules.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/BlackDark/renovate-server/internal/config"
	"github.com/BlackDark/renovate-server/internal/dispatch"
	"github.com/BlackDark/renovate-server/internal/executor"
	dockerexec "github.com/BlackDark/renovate-server/internal/executor/docker"
	"github.com/BlackDark/renovate-server/internal/executor/gitlabci"
	kubeexec "github.com/BlackDark/renovate-server/internal/executor/kubernetes"
	"github.com/BlackDark/renovate-server/internal/metrics"
	"github.com/BlackDark/renovate-server/internal/platform"
	githubplatform "github.com/BlackDark/renovate-server/internal/platform/github"
	gitlabplatform "github.com/BlackDark/renovate-server/internal/platform/gitlab"
	"github.com/BlackDark/renovate-server/internal/schedule"
	"github.com/BlackDark/renovate-server/internal/server"
	"github.com/BlackDark/renovate-server/internal/store"
)

var version = "dev" // set via -ldflags "-X main.version=..."

func main() {
	configPath := flag.String("config", "/etc/renovate-server/config.yaml", "path to config file")
	showVersion := flag.Bool("version", false, "print version and exit")
	flag.Parse()

	if *showVersion {
		fmt.Println("renovate-server", version)
		return
	}

	if err := run(*configPath); err != nil {
		slog.Error("fatal", "error", err)
		os.Exit(1)
	}
}

func run(configPath string) error {
	cfg, err := config.Load(configPath)
	if err != nil {
		return err
	}

	log, err := newLogger(cfg.Server.Log)
	if err != nil {
		return err
	}
	slog.SetDefault(log)
	log.Info("starting renovate-server", "version", version)

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	// Platforms.
	platforms := make([]platform.Platform, 0, len(cfg.Platforms))
	gitlabPlatforms := map[string]*gitlabplatform.GitLab{}
	for _, pc := range cfg.Platforms {
		switch pc.Type {
		case config.PlatformGitLab:
			p, err := gitlabplatform.New(pc, log)
			if err != nil {
				return fmt.Errorf("platform %q: %w", pc.Name, err)
			}
			platforms = append(platforms, p)
			gitlabPlatforms[pc.Name] = p
		case config.PlatformGitHub:
			p, err := githubplatform.New(pc, log)
			if err != nil {
				return fmt.Errorf("platform %q: %w", pc.Name, err)
			}
			platforms = append(platforms, p)
		}
	}

	// Executors.
	executors := map[string]executor.Executor{}
	for _, ec := range cfg.Executors {
		switch ec.Type {
		case config.ExecutorGitLabPipeline:
			gl, ok := gitlabPlatforms[ec.Platform]
			if !ok {
				return fmt.Errorf("executor %q: platform %q is not a configured gitlab platform", ec.Name, ec.Platform)
			}
			ex, err := gitlabci.New(ec, gl.Client(), log)
			if err != nil {
				return err
			}
			executors[ec.Name] = ex
		case config.ExecutorKubernetes:
			client, err := kubeexec.NewClientFromEnv()
			if err != nil {
				return fmt.Errorf("executor %q: %w", ec.Name, err)
			}
			executors[ec.Name] = kubeexec.New(ec, client, log)
		case config.ExecutorDocker:
			api, err := dockerexec.NewAPIFromEnv()
			if err != nil {
				return fmt.Errorf("executor %q: %w", ec.Name, err)
			}
			executors[ec.Name] = dockerexec.New(ec, api, log)
		}
	}

	// Core.
	st := store.NewMemory()
	reg := prometheus.NewRegistry()
	m := metrics.New(reg, st)
	router, err := dispatch.NewRouter(cfg.Rules, executors)
	if err != nil {
		return err
	}
	disp := dispatch.NewDispatcher(st, router, dispatch.Options{
		Debounce:      cfg.Server.Debounce,
		RunTimeout:    cfg.Server.RunTimeout,
		MaxConcurrent: cfg.Server.MaxConcurrentRuns,
		Log:           log,
		Metrics:       m,
	})

	// Re-adopt in-flight runs (kubernetes executor).
	for name, ex := range executors {
		adoptable, ok := ex.(executor.Adoptable)
		if !ok {
			continue
		}
		runs, err := adoptable.AdoptRunning(ctx)
		if err != nil {
			log.Warn("re-adoption failed, relying on run timeout", "executor", name, "error", err)
			continue
		}
		for _, run := range runs {
			disp.Adopt(run, name)
		}
	}

	// Cron schedules.
	sched := schedule.New(log)
	for _, p := range platforms {
		p := p
		err := sched.AddPlatform(p.Schedule(), func() {
			log.Info("cron discovery started", "platform", p.Name())
			repos, err := p.DiscoverRepos(ctx)
			if err != nil {
				log.Error("cron discovery failed", "platform", p.Name(), "error", err)
				return
			}
			log.Info("cron discovery finished", "platform", p.Name(), "repos", len(repos))
			for _, repo := range repos {
				disp.Enqueue(platform.Event{Repo: repo, Reason: platform.ReasonCron})
			}
		})
		if err != nil {
			return fmt.Errorf("platform %q: %w", p.Name(), err)
		}
	}
	sched.Start()

	// HTTP server.
	srv := server.New(platforms, disp, st, reg, m, log)
	httpServer := &http.Server{
		Addr:              cfg.Server.Listen,
		Handler:           srv.Handler(),
		ReadHeaderTimeout: 10 * time.Second,
	}
	serveErr := make(chan error, 1)
	go func() {
		log.Info("listening", "addr", cfg.Server.Listen)
		if err := httpServer.ListenAndServe(); !errors.Is(err, http.ErrServerClosed) {
			serveErr <- err
		}
	}()
	srv.SetReady(true)

	select {
	case err := <-serveErr:
		return err
	case <-ctx.Done():
	}

	log.Info("shutting down")
	srv.SetReady(false)
	sched.Stop()
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	_ = httpServer.Shutdown(shutdownCtx)
	if err := disp.Shutdown(shutdownCtx); err != nil {
		log.Warn("runs still in flight at shutdown", "error", err)
	}
	return nil
}

func newLogger(cfg config.Log) (*slog.Logger, error) {
	var level slog.Level
	if err := level.UnmarshalText([]byte(cfg.Level)); err != nil {
		return nil, fmt.Errorf("invalid log level %q", cfg.Level)
	}
	opts := &slog.HandlerOptions{Level: level}
	var handler slog.Handler
	switch cfg.Format {
	case "json":
		handler = slog.NewJSONHandler(os.Stderr, opts)
	case "text":
		handler = slog.NewTextHandler(os.Stderr, opts)
	default:
		return nil, fmt.Errorf("invalid log format %q", cfg.Format)
	}
	return slog.New(handler), nil
}
```

- [ ] **Step 6: Write `examples/config.yaml`**

```yaml
# Example renovate-server configuration.
# ${VAR} references are expanded from the environment at startup;
# unset variables are a fatal error.
server:
  listen: ":8080"
  log:
    level: info
    format: json
  debounce: 10s
  maxConcurrentRuns: 4
  runTimeout: 60m

platforms:
  - name: company-gitlab
    type: gitlab
    baseURL: https://gitlab.company.io
    token: ${GITLAB_TOKEN}
    botEmail: renovate-bot@company.io
    webhook:
      path: /webhook/gitlab
      secret: ${GITLAB_WEBHOOK_SECRET}
    events: [merge_request, issue]   # add "push" to react to default-branch pushes
    discovery:
      groups: [my-top-group]
      excludeArchived: true
    schedule:
      crontabs: ["0 3 * * *"]
      timezone: Europe/Berlin

executors:
  # Triggers a pipeline in a central renovate runner project.
  - name: ci-trigger
    type: gitlabPipeline
    platform: company-gitlab
    project: infra/renovate-runner
    ref: main
    triggerToken: ${PIPELINE_TRIGGER_TOKEN}
    variables:
      RENOVATE_REPO: "{{ .Repo }}"
      TRIGGER_REASON: "{{ .Reason }}"
    pollInterval: 15s

  # Spawns kubernetes jobs running the renovate image.
  # - name: k8s
  #   type: kubernetes
  #   namespace: renovate
  #   image: renovate/renovate:41
  #   cachePVC: renovate-cache
  #   jobTTL: 1h
  #   env:
  #     RENOVATE_PLATFORM: gitlab
  #     RENOVATE_ENDPOINT: https://gitlab.company.io/api/v4
  #     RENOVATE_TOKEN: ${RENOVATE_TOKEN}
  #     RENOVATE_REDIS_URL: ${REDIS_URL}

  # Runs renovate containers on the local docker daemon.
  # - name: local-docker
  #   type: docker
  #   image: renovate/renovate:41
  #   cacheVolume: renovate-cache
  #   pull: true
  #   env:
  #     RENOVATE_PLATFORM: gitlab
  #     RENOVATE_ENDPOINT: https://gitlab.company.io/api/v4
  #     RENOVATE_TOKEN: ${RENOVATE_TOKEN}

rules:  # first match wins; a catch-all "**" rule is required
  - match: "my-top-group/legacy/**"
    disabled: true
  - match: "**"
    executor: ci-trigger
```

- [ ] **Step 7: Verify build + smoke test**

```bash
go build ./...
go vet ./...
go test -race ./...
# smoke: binary starts, serves health, rejects bad webhook auth
go build -o /tmp/renovate-server-smoke ./cmd/renovate-server
GITLAB_TOKEN=x GITLAB_WEBHOOK_SECRET=hooksecret PIPELINE_TRIGGER_TOKEN=y \
  /tmp/renovate-server-smoke -config examples/config.yaml &
sleep 1
curl -sf http://localhost:8080/healthz && echo healthz-ok
curl -sf http://localhost:8080/readyz && echo readyz-ok
curl -s -o /dev/null -w '%{http_code}\n' -X POST http://localhost:8080/webhook/gitlab \
  -H 'X-Gitlab-Token: wrong' -d '{}'   # expect 401
curl -sf http://localhost:8080/api/v1/status && echo status-ok
kill %1
```
Expected: healthz-ok, readyz-ok, `401`, status JSON.

- [ ] **Step 8: Commit**

```bash
git add internal/schedule cmd examples
git commit -m "feat: add cron scheduler, main wiring and example config"
```

---

### Task 14: golangci-lint + Makefile

**Files:**
- Create: `.golangci.yml`, `Makefile`

**Interfaces:**
- Produces: `make lint`, `make test`, `make build` used by CI (Task 17).

- [ ] **Step 1: Install golangci-lint v2 (if missing) and create `.golangci.yml`**

```bash
command -v golangci-lint || brew install golangci-lint
golangci-lint version   # expect v2.x
```

`.golangci.yml` (v2 config format):

```yaml
version: "2"

linters:
  default: standard
  enable:
    - copyloopvar
    - gocritic
    - gosec
    - misspell
    - nolintlint
    - prealloc
    - revive
    - unconvert
    - unparam
    - whitespace
  settings:
    gosec:
      excludes:
        - G404 # math/rand is not used for crypto here
  exclusions:
    rules:
      - path: _test\.go
        linters:
          - gosec
          - unparam

formatters:
  enable:
    - gofmt
    - goimports
```

If `golangci-lint` on the machine is v1.x, migrate with `golangci-lint migrate` or install v2 via `go install github.com/golangci/golangci-lint/v2/cmd/golangci-lint@latest`.

- [ ] **Step 2: Create `Makefile`**

```makefile
BINARY  := renovate-server
VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
LDFLAGS := -s -w -X main.version=$(VERSION)

.PHONY: all build test lint cover docker clean

all: lint test build

build:
	CGO_ENABLED=0 go build -trimpath -ldflags "$(LDFLAGS)" -o $(BINARY) ./cmd/renovate-server

test:
	go test -race ./...

cover:
	go test -race -coverprofile=coverage.out ./...
	go tool cover -func=coverage.out | tail -1

lint:
	golangci-lint run

docker:
	docker build -t renovate-server:$(VERSION) --build-arg VERSION=$(VERSION) .

clean:
	rm -f $(BINARY) coverage.out
```

- [ ] **Step 3: Run lint and fix all findings**

Run: `make lint`
Expected: exit 0. Fix any findings in the source (not with nolint comments unless a finding is a true false-positive).

Run: `make test && make build`
Expected: PASS, binary builds.

- [ ] **Step 4: Commit**

```bash
git add .golangci.yml Makefile
git commit -m "chore: add golangci-lint config and makefile"
```

---

### Task 15: Dockerfile + compose example

**Files:**
- Create: `Dockerfile`, `.dockerignore`, `examples/docker-compose.yaml`

- [ ] **Step 1: Create `Dockerfile`**

```dockerfile
# syntax=docker/dockerfile:1
FROM golang:1.26-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN --mount=type=cache,target=/go/pkg/mod go mod download
COPY . .
ARG VERSION=dev
RUN --mount=type=cache,target=/go/pkg/mod \
    --mount=type=cache,target=/root/.cache/go-build \
    CGO_ENABLED=0 go build -trimpath \
      -ldflags "-s -w -X main.version=${VERSION}" \
      -o /out/renovate-server ./cmd/renovate-server

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/renovate-server /renovate-server
USER 65532:65532
EXPOSE 8080
ENTRYPOINT ["/renovate-server"]
CMD ["-config", "/etc/renovate-server/config.yaml"]
```

- [ ] **Step 2: Create `.dockerignore`**

```
.git
docs
examples
deploy
*.md
coverage.out
renovate-server
dist
```

- [ ] **Step 3: Create `examples/docker-compose.yaml`**

```yaml
services:
  renovate-server:
    image: ghcr.io/blackdark/renovate-server:latest
    restart: unless-stopped
    ports:
      - "8080:8080"
    environment:
      GITLAB_TOKEN: ${GITLAB_TOKEN:?required}
      GITLAB_WEBHOOK_SECRET: ${GITLAB_WEBHOOK_SECRET:?required}
      PIPELINE_TRIGGER_TOKEN: ${PIPELINE_TRIGGER_TOKEN:?required}
    volumes:
      - ./config.yaml:/etc/renovate-server/config.yaml:ro
      # Only needed for the docker executor:
      # - /var/run/docker.sock:/var/run/docker.sock
    read_only: true
    security_opt:
      - no-new-privileges:true
    healthcheck:
      test: ["CMD", "/renovate-server", "-version"]
      interval: 30s
      timeout: 3s

volumes:
  renovate-cache:
```

Note: the distroless image has no shell/wget; the healthcheck uses the binary's `-version` as liveness proxy. For real HTTP health checks rely on an external monitor or k8s probes.

- [ ] **Step 4: Build and smoke-test the image**

```bash
docker build -t renovate-server:dev .
docker run --rm renovate-server:dev -version           # prints version
docker run --rm --entrypoint "" renovate-server:dev /renovate-server -version 2>/dev/null || true
# verify non-root:
docker inspect renovate-server:dev --format '{{.Config.User}}'   # expect 65532:65532
```
Expected: version prints, user is 65532:65532.

- [ ] **Step 5: Commit**

```bash
git add Dockerfile .dockerignore examples/docker-compose.yaml
git commit -m "feat: add hardened dockerfile and compose example"
```

---

### Task 16: Helm chart

**Files:**
- Create: `deploy/chart/renovate-server/Chart.yaml`, `values.yaml`, `templates/_helpers.tpl`, `templates/deployment.yaml`, `templates/service.yaml`, `templates/serviceaccount.yaml`, `templates/rbac.yaml`, `templates/configmap.yaml`, `templates/pvc.yaml`, `templates/ingress.yaml`

Chart requirements (write standard Helm boilerplate; key decisions listed here):

- `Chart.yaml`: apiVersion v2, name `renovate-server`, version `0.1.0`, appVersion `0.1.0`.
- `values.yaml`:

```yaml
image:
  repository: ghcr.io/blackdark/renovate-server
  tag: ""            # defaults to appVersion
  pullPolicy: IfNotPresent

replicaCount: 1      # in-memory store: keep 1

config: {}           # rendered verbatim into the ConfigMap as config.yaml

# Secrets are injected as env vars referenced by ${VAR} in config.
envFrom: []
# - secretRef:
#     name: renovate-server-secrets

rbac:
  # Create Role/RoleBinding for the kubernetes executor (Jobs).
  create: true
  jobNamespace: ""   # defaults to release namespace

cache:
  pvc:
    create: false
    name: renovate-cache
    size: 10Gi
    storageClassName: ""

service:
  type: ClusterIP
  port: 8080

ingress:
  enabled: false
  className: ""
  annotations: {}
  hosts: []
  tls: []

resources:
  requests: {cpu: 50m, memory: 64Mi}
  limits: {memory: 256Mi}

podSecurityContext:
  runAsNonRoot: true
  runAsUser: 65532
  seccompProfile: {type: RuntimeDefault}
securityContext:
  allowPrivilegeEscalation: false
  readOnlyRootFilesystem: true
  capabilities: {drop: [ALL]}
```

- `templates/deployment.yaml`: mounts the ConfigMap at `/etc/renovate-server/`, liveness `GET /healthz`, readiness `GET /readyz`, port 8080, `envFrom` passthrough, checksum/config annotation for restarts on config change.
- `templates/rbac.yaml` (when `rbac.create`): Role in `rbac.jobNamespace | default .Release.Namespace` with rules: apiGroups `["batch"]`, resources `["jobs"]`, verbs `["create","get","list","watch","delete"]` — nothing more. RoleBinding to the chart ServiceAccount.
- `templates/pvc.yaml` (when `cache.pvc.create`): RWX not assumed — default `ReadWriteOnce`, override via `cache.pvc.accessModes`.

- [ ] **Step 1: Write all chart files per the decisions above** (standard Helm patterns, `helm create`-style helpers trimmed to what's used)

- [ ] **Step 2: Validate**

```bash
helm lint deploy/chart/renovate-server
helm template test deploy/chart/renovate-server \
  --set config.server.listen=":8080" | kubectl apply --dry-run=client -f - || true
```
Expected: `helm lint` passes with 0 failures. (kubectl dry-run only if a cluster/kubectl is available; otherwise skip.)
If `helm` is not installed: `brew install helm`.

- [ ] **Step 3: Commit**

```bash
git add deploy
git commit -m "feat: add helm chart with minimal rbac and hardened pod security"
```

---

### Task 17: GitHub Actions CI + zizmor + goreleaser + renovate config

**Files:**
- Create: `.github/workflows/ci.yml`, `.github/workflows/release.yml`, `.github/zizmor.yml`, `.goreleaser.yaml`, `.renovaterc.json`

Action pinning: zizmor flags unpinned actions. Resolve each action tag to a commit SHA at implementation time:

```bash
gh api repos/actions/checkout/git/ref/tags/v5 --jq .object.sha
gh api repos/actions/setup-go/git/ref/tags/v6 --jq .object.sha
gh api repos/golangci/golangci-lint-action/git/ref/tags/v8 --jq .object.sha
gh api repos/goreleaser/goreleaser-action/git/ref/tags/v6 --jq .object.sha
gh api repos/docker/login-action/git/ref/tags/v3 --jq .object.sha
gh api repos/docker/setup-buildx-action/git/ref/tags/v3 --jq .object.sha
gh api repos/docker/setup-qemu-action/git/ref/tags/v3 --jq .object.sha
gh api repos/docker/build-push-action/git/ref/tags/v6 --jq .object.sha
gh api repos/docker/metadata-action/git/ref/tags/v5 --jq .object.sha
gh api repos/zizmorcore/zizmor-action/git/ref/tags/v0.2.0 --jq .object.sha
```

If a tag doesn't exist (major bumped), list tags with `gh api repos/<owner>/<repo>/tags --jq '.[].name' | head` and take the latest stable. Use the form `uses: actions/checkout@<sha> # v5`.

- [ ] **Step 1: Create `.github/workflows/ci.yml`**

```yaml
name: CI

on:
  push:
    branches: [main]
  pull_request:

permissions: {}

jobs:
  lint:
    runs-on: ubuntu-latest
    permissions:
      contents: read
    steps:
      - uses: actions/checkout@<SHA> # v5
        with:
          persist-credentials: false
      - uses: actions/setup-go@<SHA> # v6
        with:
          go-version-file: go.mod
      - uses: golangci/golangci-lint-action@<SHA> # v8

  test:
    runs-on: ubuntu-latest
    permissions:
      contents: read
    steps:
      - uses: actions/checkout@<SHA> # v5
        with:
          persist-credentials: false
      - uses: actions/setup-go@<SHA> # v6
        with:
          go-version-file: go.mod
      - run: go test -race -coverprofile=coverage.out ./...
      - run: go tool cover -func=coverage.out | tail -1

  build:
    runs-on: ubuntu-latest
    permissions:
      contents: read
    steps:
      - uses: actions/checkout@<SHA> # v5
        with:
          persist-credentials: false
      - uses: actions/setup-go@<SHA> # v6
        with:
          go-version-file: go.mod
      - run: make build
      - run: docker build -t renovate-server:ci .

  zizmor:
    runs-on: ubuntu-latest
    permissions:
      contents: read
      security-events: write
    steps:
      - uses: actions/checkout@<SHA> # v5
        with:
          persist-credentials: false
      - uses: zizmorcore/zizmor-action@<SHA> # v0.2.0
```

Replace every `<SHA>` with the real resolved SHA. If `zizmorcore/zizmor-action` is unavailable, fall back to `pip install zizmor && zizmor .github/workflows/`.

- [ ] **Step 2: Create `.github/workflows/release.yml`**

```yaml
name: Release

on:
  push:
    tags: ["v*"]

permissions: {}

jobs:
  goreleaser:
    runs-on: ubuntu-latest
    permissions:
      contents: write
    steps:
      - uses: actions/checkout@<SHA> # v5
        with:
          fetch-depth: 0
          persist-credentials: false
      - uses: actions/setup-go@<SHA> # v6
        with:
          go-version-file: go.mod
      - uses: goreleaser/goreleaser-action@<SHA> # v6
        with:
          args: release --clean
        env:
          GITHUB_TOKEN: ${{ secrets.GITHUB_TOKEN }}

  image:
    runs-on: ubuntu-latest
    permissions:
      contents: read
      packages: write
    steps:
      - uses: actions/checkout@<SHA> # v5
        with:
          persist-credentials: false
      - uses: docker/setup-qemu-action@<SHA> # v3
      - uses: docker/setup-buildx-action@<SHA> # v3
      - uses: docker/login-action@<SHA> # v3
        with:
          registry: ghcr.io
          username: ${{ github.actor }}
          password: ${{ secrets.GITHUB_TOKEN }}
      - id: meta
        uses: docker/metadata-action@<SHA> # v5
        with:
          images: ghcr.io/blackdark/renovate-server
          tags: |
            type=semver,pattern={{version}}
            type=semver,pattern={{major}}.{{minor}}
      - uses: docker/build-push-action@<SHA> # v6
        with:
          context: .
          platforms: linux/amd64,linux/arm64
          push: true
          build-args: |
            VERSION=${{ github.ref_name }}
          tags: ${{ steps.meta.outputs.tags }}
          labels: ${{ steps.meta.outputs.labels }}
```

- [ ] **Step 3: Create `.goreleaser.yaml`**

```yaml
version: 2

builds:
  - main: ./cmd/renovate-server
    binary: renovate-server
    env: [CGO_ENABLED=0]
    flags: [-trimpath]
    ldflags: ["-s -w -X main.version={{.Version}}"]
    goos: [linux, darwin]
    goarch: [amd64, arm64]

archives:
  - formats: [tar.gz]

checksum:
  name_template: checksums.txt

changelog:
  use: github-native
```

- [ ] **Step 4: Create `.github/zizmor.yml`**

```yaml
rules:
  unpinned-uses:
    config:
      policies:
        "*": hash-pin
```

- [ ] **Step 5: Create `.renovaterc.json`**

```json
{
  "$schema": "https://docs.renovatebot.com/renovate-schema.json",
  "extends": ["config:recommended", ":semanticCommits"],
  "postUpdateOptions": ["gomodTidy"],
  "packageRules": [
    {
      "matchManagers": ["gomod"],
      "matchUpdateTypes": ["minor", "patch"],
      "groupName": "go dependencies"
    }
  ]
}
```

- [ ] **Step 6: Validate locally**

```bash
# zizmor (install once): pipx install zizmor OR brew install zizmor
zizmor .github/workflows/
# goreleaser config check:
go run github.com/goreleaser/goreleaser/v2@latest check
```
Expected: zizmor exits 0 (no findings at default severity); goreleaser check passes.

- [ ] **Step 7: Commit**

```bash
git add .github .goreleaser.yaml .renovaterc.json
git commit -m "ci: add github actions, zizmor, goreleaser and renovate config"
```

---

### Task 18: README + final verification

**Files:**
- Create: `README.md`

- [ ] **Step 1: Write `README.md`** covering:
- What it is: coordinator that triggers Renovate runs per repo on webhook/cron; it does not run Renovate in-process.
- Feature list: GitLab group webhooks + GitHub org webhooks (MR/PR checkbox, dashboard issue checkbox, optional push), cron discovery, three executors, per-repo locking + coalescing, debounce, run timeout, Prometheus metrics, status API.
- Quick start: docker compose snippet + minimal config (reference `examples/config.yaml`).
- Configuration reference: table of all config keys with type/default/description (derive from `internal/config/config.go` — every field documented).
- Webhook setup: GitLab group webhook (URL, secret token, which triggers to enable: Issues events, Merge request events, Push events), GitHub org webhook (JSON, secret, events: issues, pull_request, push).
- Executor docs: what each executor needs (trigger token + runner project; RBAC + PVC; docker socket) and how Renovate itself gets configured (env passthrough, `RENOVATE_REPOSITORIES` is set by the server).
- Caching section: PVC/volume file cache, `RENOVATE_REDIS_URL` for package cache.
- Operations: endpoints table (healthz/readyz/metrics/status), metric names, restart semantics (k8s re-adoption, timeout heals lost pipeline/docker tracking, single replica).
- Deployment: Helm chart pointer, compose pointer.
- Development: make targets, test instructions.

- [ ] **Step 2: Full final verification**

```bash
make lint
go test -race -coverprofile=coverage.out ./...
go tool cover -func=coverage.out | tail -1   # expect meaningful total (>70%)
make build && ./renovate-server -version
docker build -t renovate-server:final .
helm lint deploy/chart/renovate-server
zizmor .github/workflows/
```
Expected: all green. Fix anything that isn't before committing.

- [ ] **Step 3: Commit**

```bash
git add README.md
git commit -m "docs: add readme with configuration and operations reference"
```

- [ ] **Step 4: Use superpowers:verification-before-completion, then superpowers:finishing-a-development-branch**

---

## Plan Self-Review Notes

- Spec coverage: webhooks (Tasks 7, 8, 12), executors (9, 10, 11), locking/coalescing/debounce/timeout (4, 6), cron discovery (13), caching passthrough (10, 11, config), config+validation (2), store interface for Redis later (4), observability (12), tests (every task), Docker (15), Helm+RBAC (16), CI/zizmor/goreleaser/renovate (17), README (18). Push events: implemented opt-in via `events` list — matches "consider it if doable".
- Library API drift risk: gitlab client-go v1.46, go-github v76, docker SDK v28 and client-go v0.36 APIs are pinned but may differ in detail from the snippets. Tests define behavior; adapt call sites, not tests, on compile errors.
- Type consistency verified: `store.Store` (Queue/StartRun/FinishRun/Adopt/Snapshot), `platform.Platform` (Name/WebhookPath/ParseWebhook/DiscoverRepos/Schedule), `executor.Executor` (Name/Run), `dispatch.Metrics` implemented by `metrics.Metrics`, `server.Enqueuer` implemented by `dispatch.Dispatcher`.





