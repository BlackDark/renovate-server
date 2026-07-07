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
