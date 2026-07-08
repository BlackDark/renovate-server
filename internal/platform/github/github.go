// Package github adapts GitHub (cloud or enterprise) to the platform
// interface: org webhook parsing and repo discovery.
package github

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"slices"

	gogithub "github.com/google/go-github/v76/github"

	"github.com/BlackDark/renovate-server/internal/config"
	"github.com/BlackDark/renovate-server/internal/platform"
)

// GitHub implements platform.Platform for github.com or GitHub Enterprise.
type GitHub struct {
	name                string
	client              *gogithub.Client
	webhookPath         string
	secret              []byte
	botEmail            string
	dashboardIssueTitle string
	allowAnyCheckbox    bool
	mrFilter            config.MRFilter
	events              map[string]bool
	orgs                []string
	excludeArchived     bool
	schedule            config.Schedule
	log                 *slog.Logger
}

// New builds a GitHub adapter from its platform config section.
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
		name:                cfg.Name,
		client:              client,
		webhookPath:         cfg.Webhook.Path,
		secret:              []byte(cfg.Webhook.Secret),
		botEmail:            cfg.BotEmail,
		dashboardIssueTitle: cfg.DashboardIssueTitle,
		allowAnyCheckbox:    cfg.AllowAnyCheckbox,
		mrFilter:            cfg.MRFilter,
		events:              events,
		orgs:                cfg.Discovery.Groups,
		excludeArchived:     cfg.Discovery.ExcludeArchived,
		schedule:            cfg.Schedule,
		log:                 log.With("platform", cfg.Name),
	}, nil
}

// Name returns the platform's configured name.
func (g *GitHub) Name() string { return g.name }

// WebhookPath returns the HTTP path this platform's webhooks arrive on.
func (g *GitHub) WebhookPath() string { return g.webhookPath }

// Schedule returns the platform's cron configuration.
func (g *GitHub) Schedule() config.Schedule { return g.schedule }

// ParseWebhook verifies the HMAC signature and maps supported events
// (edited PR/issue with newly checked boxes, default-branch push) to a
// run request; unsupported or irrelevant events return (nil, nil).
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
		if !g.prTicked(ev) {
			return nil, nil
		}
		return g.event(ev.GetRepo().GetFullName(), platform.ReasonMergeRequest), nil

	case *gogithub.IssuesEvent:
		if !g.events["issue"] || ev.GetAction() != "edited" {
			return nil, nil
		}
		// A matching title identifies renovate's dependency dashboard; any
		// checkbox tick inside it triggers (dashboard checkboxes carry no
		// reliable per-item markers on all renovate versions).
		identified := g.allowAnyCheckbox || g.dashboardIssueTitle == "*" ||
			ev.GetIssue().GetTitle() == g.dashboardIssueTitle ||
			platform.HasRenovateDebugMarker(ev.GetIssue().GetBody())
		if !identified {
			return nil, nil
		}
		if !tickedPlain(previousBody(ev.GetChanges()), ev.GetIssue().GetBody()) {
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

// tickedPlain reports whether the number of checked todo items (of any
// kind) increased between the previous and current body.
func tickedPlain(previous, current string) bool {
	if current == "" {
		return false
	}
	return platform.CheckedItems(current) > platform.CheckedItems(previous)
}

// prTicked decides whether a PR body edit is a renovate checkbox tick. PRs
// identified as renovate PRs (debug marker, head branch or author) trigger
// on any checkbox; others need per-checkbox markers.
func (g *GitHub) prTicked(ev *gogithub.PullRequestEvent) bool {
	previous, current := previousBody(ev.GetChanges()), ev.GetPullRequest().GetBody()
	if current == "" {
		return false
	}
	if g.allowAnyCheckbox || g.isRenovatePR(ev) {
		return platform.CheckedItems(current) > platform.CheckedItems(previous)
	}
	return platform.CheckedMarkerItems(current) > platform.CheckedMarkerItems(previous)
}

func (g *GitHub) isRenovatePR(ev *gogithub.PullRequestEvent) bool {
	pr := ev.GetPullRequest()
	if platform.HasRenovateDebugMarker(pr.GetBody()) {
		return true
	}
	if platform.BranchHasPrefix(g.mrFilter.SourceBranchPrefixes, pr.GetHead().GetRef()) {
		return true
	}
	return len(g.mrFilter.Authors) > 0 && slices.Contains(g.mrFilter.Authors, pr.GetUser().GetLogin())
}

func (g *GitHub) event(fullName string, reason platform.Reason) *platform.Event {
	if !platform.RepoAllowed(g.orgs, fullName) {
		g.log.Warn("webhook for repo outside configured orgs ignored", "repo", fullName)
		return nil
	}
	return &platform.Event{
		Repo:   platform.Repo{Platform: g.name, FullName: fullName},
		Reason: reason,
	}
}

// DiscoverRepos lists all repos of the configured orgs, honoring the
// archived filter.
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
