// Package config defines the YAML configuration schema, loading with
// ${VAR} env expansion, defaults and fail-fast validation.
package config

import "time"

// Platform and executor type identifiers used in the config file.
const (
	PlatformGitLab = "gitlab"
	PlatformGitHub = "github"

	ExecutorGitLabPipeline = "gitlabPipeline"
	ExecutorKubernetes     = "kubernetes"
	ExecutorDocker         = "docker"
	ExecutorNoop           = "noop"
)

// Config is the root of the configuration file.
type Config struct {
	Server    Server     `yaml:"server"`
	Platforms []Platform `yaml:"platforms"`
	Executors []Executor `yaml:"executors"`
	Rules     []Rule     `yaml:"rules"`
}

// Server holds process-wide settings: listener, logging and dispatch tuning.
type Server struct {
	Listen            string        `yaml:"listen"`
	Log               Log           `yaml:"log"`
	Debounce          time.Duration `yaml:"debounce"`
	MaxConcurrentRuns int           `yaml:"maxConcurrentRuns"`
	RunTimeout        time.Duration `yaml:"runTimeout"`
	// HistorySize is the number of finished runs kept for /api/v1/runs.
	HistorySize int         `yaml:"historySize"`
	Store       StoreConfig `yaml:"store"`
}

// StoreConfig selects where repo run state lives.
type StoreConfig struct {
	Type  string      `yaml:"type"` // memory (default) | redis
	Redis RedisConfig `yaml:"redis"`
}

// RedisConfig configures the redis-backed store.
type RedisConfig struct {
	URL       string        `yaml:"url"`       // redis://[:pass@]host:port/db
	KeyPrefix string        `yaml:"keyPrefix"` // default "renovate-server:"
	TTL       time.Duration `yaml:"ttl"`       // default 2h; stale entries self-heal
}

// Log configures slog output.
type Log struct {
	Level  string `yaml:"level"`  // debug|info|warn|error
	Format string `yaml:"format"` // json|text
}

// Platform describes one git hosting platform (GitLab instance or GitHub
// org) with its webhook, discovery and schedule settings.
type Platform struct {
	Name     string `yaml:"name"`
	Type     string `yaml:"type"` // gitlab|github
	BaseURL  string `yaml:"baseURL"`
	Token    string `yaml:"token"`
	BotEmail string `yaml:"botEmail"` // push events from this author are ignored
	// DashboardIssueTitle filters issue events: only issues with this title
	// trigger runs. "*" disables the filter. Default "Dependency Dashboard".
	DashboardIssueTitle string `yaml:"dashboardIssueTitle"`
	// AllowAnyCheckbox reverts to triggering on any checked markdown todo
	// item instead of requiring Renovate's HTML comment markers.
	AllowAnyCheckbox bool      `yaml:"allowAnyCheckbox"`
	MRFilter         MRFilter  `yaml:"mrFilter"`
	Webhook          Webhook   `yaml:"webhook"`
	Events           []string  `yaml:"events"` // merge_request|issue|push
	Discovery        Discovery `yaml:"discovery"`
	Schedule         Schedule  `yaml:"schedule"`
}

// MRFilter identifies renovate MRs/PRs so checkbox ticks inside them
// trigger runs even without per-checkbox markers. An MR counts as a
// renovate MR when ANY signal matches: renovate-debug marker in the
// description (always checked), source branch prefix, or author.
type MRFilter struct {
	// SourceBranchPrefixes match the MR source branch. Default ["renovate/"].
	SourceBranchPrefixes []string `yaml:"sourceBranchPrefixes"`
	// Authors match the MR/PR author username (GitLab: resolved via API and
	// cached; GitHub: login from the payload). Empty = signal disabled.
	Authors []string `yaml:"authors"`
}

// Webhook is the receiving endpoint for a platform's webhooks.
type Webhook struct {
	Path   string `yaml:"path"`
	Secret string `yaml:"secret"`
}

// Discovery controls which repos the cron schedule enumerates.
type Discovery struct {
	Groups          []string `yaml:"groups"`
	ExcludeArchived bool     `yaml:"excludeArchived"`
}

// Schedule holds the cron expressions for periodic full runs.
type Schedule struct {
	Crontabs []string `yaml:"crontabs"`
	Timezone string   `yaml:"timezone"`
}

// Executor describes one way to run renovate; only the fields for its
// Type are used.
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
	Pod       PodConfig     `yaml:"pod"`

	// docker
	CacheVolume string `yaml:"cacheVolume"`
	Pull        bool   `yaml:"pull"`

	// kubernetes + docker: extra env for the renovate container
	Env map[string]string `yaml:"env"`
}

// PodConfig customizes the pod of kubernetes-executor Jobs.
type PodConfig struct {
	Resources          ResourceConfig    `yaml:"resources"`
	NodeSelector       map[string]string `yaml:"nodeSelector"`
	Tolerations        []Toleration      `yaml:"tolerations"`
	ServiceAccountName string            `yaml:"serviceAccountName"`
	ImagePullSecrets   []string          `yaml:"imagePullSecrets"`
	// ActiveDeadlineSeconds lets the cluster kill runaway jobs; the server's
	// runTimeout stays authoritative for the repo lock.
	ActiveDeadlineSeconds int64 `yaml:"activeDeadlineSeconds"`
}

// ResourceConfig holds k8s resource quantities as strings (e.g. "250m").
type ResourceConfig struct {
	Requests map[string]string `yaml:"requests"`
	Limits   map[string]string `yaml:"limits"`
}

// Toleration mirrors the k8s toleration fields the executor supports.
type Toleration struct {
	Key      string `yaml:"key"`
	Operator string `yaml:"operator"` // Equal|Exists
	Value    string `yaml:"value"`
	Effect   string `yaml:"effect"` // NoSchedule|PreferNoSchedule|NoExecute
}

// Rule routes repos (matched by doublestar glob on the full name) to an
// executor, or disables them. First match wins.
type Rule struct {
	Match    string `yaml:"match"`
	Executor string `yaml:"executor"`
	Disabled bool   `yaml:"disabled"`
}
