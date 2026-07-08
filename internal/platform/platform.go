// Package platform defines the platform-neutral types (repos, events) and
// the interface every git hosting adapter implements.
package platform

import (
	"context"
	"errors"
	"net/http"
	"strings"

	"github.com/BlackDark/renovate-server/internal/config"
)

// RepoAllowed reports whether a repo full name is inside one of the
// configured groups/orgs (path-prefix match, case-insensitive). With no
// groups configured, all repos are allowed.
func RepoAllowed(groups []string, fullName string) bool {
	if len(groups) == 0 {
		return true
	}
	name := strings.ToLower(fullName)
	for _, g := range groups {
		g = strings.ToLower(g)
		if name == g || strings.HasPrefix(name, g+"/") {
			return true
		}
	}
	return false
}

// Repo identifies a repository on a configured platform.
type Repo struct {
	Platform string // platform config name
	FullName string // e.g. "group/subgroup/project"
}

// Key returns the unique dispatch key for the repo.
func (r Repo) Key() string { return r.Platform + ":" + r.FullName }

// Reason describes why a run was requested.
type Reason string

// Reasons a run can be requested for.
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
