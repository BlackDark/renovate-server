package config

import (
	"fmt"
	"os"
	"regexp"
	"time"

	"github.com/bmatcuk/doublestar/v4"
	"github.com/robfig/cron/v3"
	"gopkg.in/yaml.v3"
	"k8s.io/apimachinery/pkg/api/resource"
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

	var root yaml.Node
	if err := yaml.Unmarshal(raw, &root); err != nil {
		return nil, fmt.Errorf("parse config: %w", err)
	}

	// Expansion happens on parsed string scalars, not raw bytes, so ${VAR}
	// references inside YAML comments are ignored.
	var missing []string
	expandNode(&root, &missing)
	if len(missing) > 0 {
		return nil, fmt.Errorf("unset environment variables referenced in config: %v", missing)
	}

	var cfg Config
	if err := root.Decode(&cfg); err != nil {
		return nil, fmt.Errorf("parse config: %w", err)
	}

	cfg.applyDefaults()
	if err := cfg.validate(); err != nil {
		return nil, fmt.Errorf("invalid config: %w", err)
	}
	return &cfg, nil
}

func expandNode(n *yaml.Node, missing *[]string) {
	if n.Kind == yaml.ScalarNode && n.Tag == "!!str" {
		n.Value = envVarPattern.ReplaceAllStringFunc(n.Value, func(m string) string {
			name := envVarPattern.FindStringSubmatch(m)[1]
			val, ok := os.LookupEnv(name)
			if !ok {
				*missing = append(*missing, name)
				return m
			}
			return val
		})
	}
	for _, c := range n.Content {
		expandNode(c, missing)
	}
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
	if c.Server.HistorySize == 0 {
		c.Server.HistorySize = 100
	}
	if c.Server.Store.Type == "" {
		c.Server.Store.Type = "memory"
	}
	if c.Server.Store.Redis.KeyPrefix == "" {
		c.Server.Store.Redis.KeyPrefix = "renovate-server:"
	}
	if c.Server.Store.Redis.TTL == 0 {
		c.Server.Store.Redis.TTL = 2 * time.Hour
	}
	for i := range c.Platforms {
		if c.Platforms[i].DashboardIssueTitle == "" {
			c.Platforms[i].DashboardIssueTitle = "Dependency Dashboard"
		}
		if c.Platforms[i].MRFilter.SourceBranchPrefixes == nil {
			c.Platforms[i].MRFilter.SourceBranchPrefixes = []string{"renovate/"}
		}
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
	switch c.Server.Store.Type {
	case "memory":
	case "redis":
		if c.Server.Store.Redis.URL == "" {
			return fmt.Errorf("store type redis requires store.redis.url")
		}
	default:
		return fmt.Errorf("store type %q is not supported", c.Server.Store.Type)
	}
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
			for _, quantities := range []map[string]string{e.Pod.Resources.Requests, e.Pod.Resources.Limits} {
				for name, val := range quantities {
					if _, err := resource.ParseQuantity(val); err != nil {
						return fmt.Errorf("executor %q: invalid resource quantity %s=%q: %w", e.Name, name, val, err)
					}
				}
			}
		case ExecutorDocker:
			if e.Image == "" {
				return fmt.Errorf("executor %q: image is required", e.Name)
			}
		case ExecutorNoop:
			// shadow mode: no configuration beyond the name
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
