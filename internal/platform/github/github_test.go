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
	"strings"
	"testing"

	"github.com/BlackDark/renovate-server/internal/config"
	"github.com/BlackDark/renovate-server/internal/platform"
)

func testConfig(baseURL string) config.Platform {
	return config.Platform{
		Name:                "gh",
		Type:                config.PlatformGitHub,
		BaseURL:             baseURL,
		Token:               "ghp_test",
		BotEmail:            "renovate@example.com",
		Webhook:             config.Webhook{Path: "/webhook/github", Secret: "s3cret"},
		Events:              []string{"merge_request", "issue", "push"},
		DashboardIssueTitle: "Dependency Dashboard",
		MRFilter:            config.MRFilter{SourceBranchPrefixes: []string{"renovate/"}},
		Discovery:           config.Discovery{Groups: []string{"my-org"}},
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

func webhookRequest(eventType, secret, body string) *http.Request {
	r := httptest.NewRequest(http.MethodPost, "/webhook/github", bytes.NewBufferString(body))
	r.Header.Set("X-GitHub-Event", eventType)
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("X-Hub-Signature-256", sign(secret, []byte(body)))
	return r
}

const prTicked = `{
  "action": "edited",
  "pull_request": {"body": "- [x] <!-- rebase-check -->rebase"},
  "changes": {"body": {"from": "- [ ] <!-- rebase-check -->rebase"}},
  "repository": {"full_name": "my-org/app", "default_branch": "main"}
}`

const prUnticked = `{
  "action": "edited",
  "pull_request": {"body": "- [ ] <!-- rebase-check -->rebase"},
  "changes": {"body": {"from": "- [x] <!-- rebase-check -->rebase"}},
  "repository": {"full_name": "my-org/app", "default_branch": "main"}
}`

const prTickedNoMarker = `{
  "action": "edited",
  "pull_request": {"body": "- [x] human task"},
  "changes": {"body": {"from": "- [ ] human task"}},
  "repository": {"full_name": "my-org/app", "default_branch": "main"}
}`

const prRenovateDebugTicked = `{
  "action": "edited",
  "pull_request": {"head": {"ref": "chore/update-foo"},
    "body": "- [x] rebase\n\n<!--renovate-debug:eyJjcmVhdGVkSW5WZXIiOiI0My4yNDEuNSJ9-->"},
  "changes": {"body": {"from": "- [ ] rebase\n\n<!--renovate-debug:eyJjcmVhdGVkSW5WZXIiOiI0My4yNDEuNSJ9-->"}},
  "repository": {"full_name": "my-org/app", "default_branch": "main"}
}`

const prRenovateBranchTicked = `{
  "action": "edited",
  "pull_request": {"head": {"ref": "renovate/golang-deps"}, "body": "- [x] rebase"},
  "changes": {"body": {"from": "- [ ] rebase"}},
  "repository": {"full_name": "my-org/app", "default_branch": "main"}
}`

const prAuthorTicked = `{
  "action": "edited",
  "pull_request": {"head": {"ref": "feature/custom"}, "user": {"login": "renovate-bot"}, "body": "- [x] rebase"},
  "changes": {"body": {"from": "- [ ] rebase"}},
  "repository": {"full_name": "my-org/app", "default_branch": "main"}
}`

const issueTicked = `{
  "action": "edited",
  "issue": {"title": "Dependency Dashboard", "body": "- [x] <!-- approve-all-pending-prs -->approve all"},
  "changes": {"body": {"from": "- [ ] <!-- approve-all-pending-prs -->approve all"}},
  "repository": {"full_name": "my-org/app", "default_branch": "main"}
}`

const issueTickedWrongTitle = `{
  "action": "edited",
  "issue": {"title": "Some Issue", "body": "- [x] <!-- approve-all-pending-prs -->approve"},
  "changes": {"body": {"from": "- [ ] <!-- approve-all-pending-prs -->approve"}},
  "repository": {"full_name": "my-org/app", "default_branch": "main"}
}`

const issueTickedWrongTitlePlain = `{
  "action": "edited",
  "issue": {"title": "Team TODO", "body": "- [x] buy coffee"},
  "changes": {"body": {"from": "- [ ] buy coffee"}},
  "repository": {"full_name": "my-org/app", "default_branch": "main"}
}`

const issueTickedPlain = `{
  "action": "edited",
  "issue": {"title": "Dependency Dashboard", "body": "- [x] Check this box to trigger a request for Renovate to run again"},
  "changes": {"body": {"from": "- [ ] Check this box to trigger a request for Renovate to run again"}},
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
		{"pr checkbox without renovate marker ignored", "pull_request", prTickedNoMarker, nil},
		{"renovate-debug marker identifies PR, plain checkbox triggers", "pull_request", prRenovateDebugTicked, &platform.Event{
			Repo:   platform.Repo{Platform: "gh", FullName: "my-org/app"},
			Reason: platform.ReasonMergeRequest,
		}},
		{"renovate branch prefix identifies PR, plain checkbox triggers", "pull_request", prRenovateBranchTicked, &platform.Event{
			Repo:   platform.Repo{Platform: "gh", FullName: "my-org/app"},
			Reason: platform.ReasonMergeRequest,
		}},
		{"author not configured: custom-branch PR ignored", "pull_request", prAuthorTicked, nil},
		{"issue with wrong title but renovate markers triggers (fallback)", "issues", issueTickedWrongTitle, &platform.Event{
			Repo:   platform.Repo{Platform: "gh", FullName: "my-org/app"},
			Reason: platform.ReasonIssue,
		}},
		{"issue with wrong title and plain checkbox ignored", "issues", issueTickedWrongTitlePlain, nil},
		{"issue checkbox ticked", "issues", issueTicked, &platform.Event{
			Repo:   platform.Repo{Platform: "gh", FullName: "my-org/app"},
			Reason: platform.ReasonIssue,
		}},
		{"dashboard issue without markers, title identifies, plain checkbox triggers", "issues", issueTickedPlain, &platform.Event{
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

func TestParseWebhookAuthorIdentifiesPR(t *testing.T) {
	cfg := testConfig("")
	cfg.MRFilter.Authors = []string{"renovate-bot"}
	g, err := New(cfg, slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatal(err)
	}
	r := webhookRequest("pull_request", "s3cret", prAuthorTicked)
	got, err := g.ParseWebhook(r, []byte(prAuthorTicked))
	if err != nil || got == nil {
		t.Fatalf("author-matched PR must trigger, got %+v, %v", got, err)
	}
}

func TestParseWebhookAllowAnyCheckbox(t *testing.T) {
	cfg := testConfig("")
	cfg.AllowAnyCheckbox = true
	g, err := New(cfg, slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatal(err)
	}
	r := webhookRequest("pull_request", "s3cret", prTickedNoMarker)
	got, err := g.ParseWebhook(r, []byte(prTickedNoMarker))
	if err != nil || got == nil {
		t.Fatalf("allowAnyCheckbox should trigger on plain checkbox, got %+v, %v", got, err)
	}
	r = webhookRequest("issues", "s3cret", issueTickedWrongTitle)
	got, err = g.ParseWebhook(r, []byte(issueTickedWrongTitle))
	if err != nil || got == nil {
		t.Fatalf("allowAnyCheckbox should skip title filter, got %+v, %v", got, err)
	}
}

func TestParseWebhookDashboardTitleWildcard(t *testing.T) {
	cfg := testConfig("")
	cfg.DashboardIssueTitle = "*"
	g, err := New(cfg, slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatal(err)
	}
	r := webhookRequest("issues", "s3cret", issueTickedWrongTitle)
	got, err := g.ParseWebhook(r, []byte(issueTickedWrongTitle))
	if err != nil || got == nil {
		t.Fatalf("wildcard title should allow any issue, got %+v, %v", got, err)
	}
}

func TestParseWebhookRejectsRepoOutsideOrgs(t *testing.T) {
	g := newTestPlatform(t, "") // orgs: [my-org]
	body := strings.ReplaceAll(prTicked, "my-org/app", "other-org/app")
	r := webhookRequest("pull_request", "s3cret", body)
	got, err := g.ParseWebhook(r, []byte(body))
	if err != nil || got != nil {
		t.Fatalf("repo outside orgs must be ignored, got %+v, %v", got, err)
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
