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
