// Package gitlab adapts a GitLab instance to the platform interface:
// group webhook parsing and project discovery.
package gitlab

import (
	"context"
	"crypto/subtle"
	"fmt"
	"log/slog"
	"net/http"
	"slices"
	"strings"
	"sync"

	gogitlab "gitlab.com/gitlab-org/api/client-go"

	"github.com/BlackDark/renovate-server/internal/config"
	"github.com/BlackDark/renovate-server/internal/platform"
)

// GitLab implements platform.Platform for a GitLab instance.
type GitLab struct {
	name                string
	client              *gogitlab.Client
	webhookPath         string
	secret              string
	botEmail            string
	dashboardIssueTitle string
	allowAnyCheckbox    bool
	mrFilter            config.MRFilter
	events              map[string]bool
	groups              []string
	excludeArchived     bool
	schedule            config.Schedule
	log                 *slog.Logger

	// usernames caches author-id -> username lookups for the MR author
	// signal; usernames change rarely enough to cache for process lifetime.
	userMu    sync.Mutex
	usernames map[int64]string
}

// New builds a GitLab adapter from its platform config section.
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
		name:                cfg.Name,
		client:              client,
		webhookPath:         cfg.Webhook.Path,
		secret:              cfg.Webhook.Secret,
		botEmail:            cfg.BotEmail,
		dashboardIssueTitle: cfg.DashboardIssueTitle,
		allowAnyCheckbox:    cfg.AllowAnyCheckbox,
		mrFilter:            cfg.MRFilter,
		usernames:           map[int64]string{},
		events:              events,
		groups:              cfg.Discovery.Groups,
		excludeArchived:     cfg.Discovery.ExcludeArchived,
		schedule:            cfg.Schedule,
		log:                 log.With("platform", cfg.Name),
	}, nil
}

// Name returns the platform's configured name.
func (g *GitLab) Name() string { return g.name }

// WebhookPath returns the HTTP path this platform's webhooks arrive on.
func (g *GitLab) WebhookPath() string { return g.webhookPath }

// Schedule returns the platform's cron configuration.
func (g *GitLab) Schedule() config.Schedule { return g.schedule }

// Client exposes the authenticated API client for the gitlabci executor.
func (g *GitLab) Client() *gogitlab.Client { return g.client }

// ParseWebhook checks the X-Gitlab-Token and maps supported events (MR or
// issue description edits with newly checked boxes, default-branch push)
// to a run request; unsupported or irrelevant events return (nil, nil).
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
		if !g.mrTicked(r.Context(), ev) {
			return nil, nil
		}
		return g.event(ev.Project.PathWithNamespace, platform.ReasonMergeRequest), nil

	case *gogitlab.IssueEvent:
		if !g.events["issue"] {
			return nil, nil
		}
		// A matching title identifies renovate's dependency dashboard; any
		// checkbox tick inside it triggers (dashboard checkboxes carry no
		// reliable per-item markers on all renovate versions).
		identified := g.allowAnyCheckbox || g.dashboardIssueTitle == "*" ||
			ev.ObjectAttributes.Title == g.dashboardIssueTitle ||
			platform.HasRenovateDebugMarker(ev.ObjectAttributes.Description)
		if !identified {
			return nil, nil
		}
		if !tickedPlain(ev.Changes.Description.Previous, ev.Changes.Description.Current) {
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

// tickedPlain reports whether the number of checked todo items (of any
// kind) increased between the previous and current description.
func tickedPlain(previous, current string) bool {
	if current == "" {
		return false
	}
	return platform.CheckedItems(current) > platform.CheckedItems(previous)
}

// mrTicked decides whether an MR description edit is a renovate checkbox
// tick. MRs identified as renovate MRs (debug marker, source branch or
// author) trigger on any checkbox; others need per-checkbox markers.
func (g *GitLab) mrTicked(ctx context.Context, ev *gogitlab.MergeEvent) bool {
	previous, current := ev.Changes.Description.Previous, ev.Changes.Description.Current
	if current == "" {
		return false
	}
	if g.allowAnyCheckbox || g.isRenovateMR(ctx, ev) {
		return platform.CheckedItems(current) > platform.CheckedItems(previous)
	}
	return platform.CheckedMarkerItems(current) > platform.CheckedMarkerItems(previous)
}

func (g *GitLab) isRenovateMR(ctx context.Context, ev *gogitlab.MergeEvent) bool {
	if platform.HasRenovateDebugMarker(ev.ObjectAttributes.Description) {
		return true
	}
	if platform.BranchHasPrefix(g.mrFilter.SourceBranchPrefixes, ev.ObjectAttributes.SourceBranch) {
		return true
	}
	if len(g.mrFilter.Authors) == 0 {
		return false
	}
	username := g.authorUsername(ctx, ev.ObjectAttributes.AuthorID)
	return username != "" && slices.Contains(g.mrFilter.Authors, username)
}

// authorUsername resolves a user id to a username via the API, cached for
// the process lifetime. Returns "" when the lookup fails.
func (g *GitLab) authorUsername(ctx context.Context, id int64) string {
	if id == 0 {
		return ""
	}
	g.userMu.Lock()
	name, ok := g.usernames[id]
	g.userMu.Unlock()
	if ok {
		return name
	}
	user, _, err := g.client.Users.GetUser(id, gogitlab.GetUsersOptions{}, gogitlab.WithContext(ctx))
	if err != nil {
		g.log.Warn("author lookup failed", "authorId", id, "error", err)
		return ""
	}
	g.userMu.Lock()
	g.usernames[id] = user.Username
	g.userMu.Unlock()
	return user.Username
}

func (g *GitLab) event(fullName string, reason platform.Reason) *platform.Event {
	if !platform.RepoAllowed(g.groups, fullName) {
		g.log.Warn("webhook for repo outside configured groups ignored", "repo", fullName)
		return nil
	}
	return &platform.Event{
		Repo:   platform.Repo{Platform: g.name, FullName: fullName},
		Reason: reason,
	}
}

// DiscoverRepos lists all projects of the configured groups including
// subgroups, honoring the archived filter.
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
