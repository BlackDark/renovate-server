package gitlab

import (
	"bytes"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/BlackDark/renovate-server/internal/config"
	"github.com/BlackDark/renovate-server/internal/platform"
)

func testConfig(baseURL string) config.Platform {
	return config.Platform{
		Name:                "gl",
		Type:                config.PlatformGitLab,
		BaseURL:             baseURL,
		Token:               "glpat-test",
		BotEmail:            "renovate@example.com",
		Webhook:             config.Webhook{Path: "/webhook/gitlab", Secret: "s3cret"},
		Events:              []string{"merge_request", "issue", "push"},
		DashboardIssueTitle: "Dependency Dashboard",
		MRFilter:            config.MRFilter{SourceBranchPrefixes: []string{"renovate/"}},
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

func webhookRequest(eventType, token, body string) *http.Request {
	r := httptest.NewRequest(http.MethodPost, "/webhook/gitlab", bytes.NewBufferString(body))
	r.Header.Set("X-Gitlab-Event", eventType)
	r.Header.Set("X-Gitlab-Token", token)
	return r
}

const mrTicked = `{
  "object_kind": "merge_request",
  "project": {"path_with_namespace": "top-group/app", "default_branch": "main"},
  "object_attributes": {"iid": 7, "action": "update", "description": "- [x] <!-- rebase-check -->rebase"},
  "changes": {"description": {"previous": "- [ ] <!-- rebase-check -->rebase", "current": "- [x] <!-- rebase-check -->rebase"}}
}`

const mrUnticked = `{
  "object_kind": "merge_request",
  "project": {"path_with_namespace": "top-group/app", "default_branch": "main"},
  "object_attributes": {"iid": 7, "action": "update", "description": "- [ ] <!-- rebase-check -->rebase"},
  "changes": {"description": {"previous": "- [x] <!-- rebase-check -->rebase", "current": "- [ ] <!-- rebase-check -->rebase"}}
}`

const mrTickedNoMarker = `{
  "object_kind": "merge_request",
  "project": {"path_with_namespace": "top-group/app", "default_branch": "main"},
  "object_attributes": {"iid": 7, "action": "update", "description": "- [x] human task"},
  "changes": {"description": {"previous": "- [ ] human task", "current": "- [x] human task"}}
}`

const issueTickedWrongTitle = `{
  "object_kind": "issue",
  "project": {"path_with_namespace": "top-group/app", "default_branch": "main"},
  "object_attributes": {"iid": 2, "action": "update", "title": "Some Issue", "description": "- [x] <!-- approve-all-pending-prs -->approve"},
  "changes": {"description": {"previous": "- [ ] <!-- approve-all-pending-prs -->approve", "current": "- [x] <!-- approve-all-pending-prs -->approve"}}
}`

// Real-world shape: no per-checkbox markers, but renovate-debug comment at
// the end of the description (as produced by current renovate versions).
const mrRenovateDebugTicked = `{
  "object_kind": "merge_request",
  "project": {"path_with_namespace": "top-group/app", "default_branch": "main"},
  "object_attributes": {"iid": 8, "action": "update", "source_branch": "chore/update-foo",
    "description": "- [x] rebase\n\n<!--renovate-debug:eyJjcmVhdGVkSW5WZXIiOiI0My4yNDEuNSJ9-->"},
  "changes": {"description": {
    "previous": "- [ ] rebase\n\n<!--renovate-debug:eyJjcmVhdGVkSW5WZXIiOiI0My4yNDEuNSJ9-->",
    "current": "- [x] rebase\n\n<!--renovate-debug:eyJjcmVhdGVkSW5WZXIiOiI0My4yNDEuNSJ9-->"}}
}`

const mrRenovateBranchTicked = `{
  "object_kind": "merge_request",
  "project": {"path_with_namespace": "top-group/app", "default_branch": "main"},
  "object_attributes": {"iid": 9, "action": "update", "source_branch": "renovate/golang-deps",
    "description": "- [x] rebase"},
  "changes": {"description": {"previous": "- [ ] rebase", "current": "- [x] rebase"}}
}`

const mrHumanTicked = `{
  "object_kind": "merge_request",
  "project": {"path_with_namespace": "top-group/app", "default_branch": "main"},
  "object_attributes": {"iid": 10, "action": "update", "source_branch": "feature/todo-list",
    "description": "- [x] human task"},
  "changes": {"description": {"previous": "- [ ] human task", "current": "- [x] human task"}}
}`

const mrAuthorTicked = `{
  "object_kind": "merge_request",
  "project": {"path_with_namespace": "top-group/app", "default_branch": "main"},
  "object_attributes": {"iid": 11, "action": "update", "source_branch": "feature/custom-branch",
    "author_id": 42, "description": "- [x] rebase"},
  "changes": {"description": {"previous": "- [ ] rebase", "current": "- [x] rebase"}}
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
  "object_attributes": {"iid": 1, "action": "update", "title": "Dependency Dashboard", "description": "- [x] <!-- approve-all-pending-prs -->approve all"},
  "changes": {"description": {"previous": "- [ ] <!-- approve-all-pending-prs -->approve all", "current": "- [x] <!-- approve-all-pending-prs -->approve all"}}
}`

// Real-world shape: dashboard issue has NO per-checkbox markers and no
// renovate-debug comment — the title is the only renovate signal.
const issueTickedPlain = `{
  "object_kind": "issue",
  "project": {"path_with_namespace": "top-group/app", "default_branch": "main"},
  "object_attributes": {"iid": 3, "action": "update", "title": "Dependency Dashboard", "description": "- [x] Check this box to trigger a request for Renovate to run again"},
  "changes": {"description": {"previous": "- [ ] Check this box to trigger a request for Renovate to run again", "current": "- [x] Check this box to trigger a request for Renovate to run again"}}
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
		{"mr checkbox without renovate marker ignored", "Merge Request Hook", mrTickedNoMarker, nil},
		{"renovate-debug marker identifies MR, plain checkbox triggers", "Merge Request Hook", mrRenovateDebugTicked, &platform.Event{
			Repo:   platform.Repo{Platform: "gl", FullName: "top-group/app"},
			Reason: platform.ReasonMergeRequest,
		}},
		{"renovate branch prefix identifies MR, plain checkbox triggers", "Merge Request Hook", mrRenovateBranchTicked, &platform.Event{
			Repo:   platform.Repo{Platform: "gl", FullName: "top-group/app"},
			Reason: platform.ReasonMergeRequest,
		}},
		{"human MR with plain checkbox ignored", "Merge Request Hook", mrHumanTicked, nil},
		{"issue with wrong title ignored", "Issue Hook", issueTickedWrongTitle, nil},
		{"issue checkbox ticked", "Issue Hook", issueTicked, &platform.Event{
			Repo:   platform.Repo{Platform: "gl", FullName: "top-group/app"},
			Reason: platform.ReasonIssue,
		}},
		{"dashboard issue without markers, title identifies, plain checkbox triggers", "Issue Hook", issueTickedPlain, &platform.Event{
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

func TestParseWebhookAllowAnyCheckbox(t *testing.T) {
	cfg := testConfig("https://gitlab.example.com")
	cfg.AllowAnyCheckbox = true
	g, err := New(cfg, slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatal(err)
	}
	// plain checkbox without marker now triggers
	r := webhookRequest("Merge Request Hook", "s3cret", mrTickedNoMarker)
	got, err := g.ParseWebhook(r, []byte(mrTickedNoMarker))
	if err != nil || got == nil {
		t.Fatalf("allowAnyCheckbox should trigger on plain checkbox, got %+v, %v", got, err)
	}
	// title filter is skipped too
	r = webhookRequest("Issue Hook", "s3cret", issueTickedWrongTitle)
	got, err = g.ParseWebhook(r, []byte(issueTickedWrongTitle))
	if err != nil || got == nil {
		t.Fatalf("allowAnyCheckbox should skip title filter, got %+v, %v", got, err)
	}
}

func TestParseWebhookDashboardTitleWildcard(t *testing.T) {
	cfg := testConfig("https://gitlab.example.com")
	cfg.DashboardIssueTitle = "*"
	g, err := New(cfg, slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatal(err)
	}
	r := webhookRequest("Issue Hook", "s3cret", issueTickedWrongTitle)
	got, err := g.ParseWebhook(r, []byte(issueTickedWrongTitle))
	if err != nil || got == nil {
		t.Fatalf("wildcard title should allow any issue, got %+v, %v", got, err)
	}
}

func TestParseWebhookRejectsRepoOutsideGroups(t *testing.T) {
	g := newTestPlatform(t, "https://gitlab.example.com") // groups: [top-group]
	body := strings.ReplaceAll(mrTicked, "top-group/app", "other-group/app")
	r := webhookRequest("Merge Request Hook", "s3cret", body)
	got, err := g.ParseWebhook(r, []byte(body))
	if err != nil || got != nil {
		t.Fatalf("repo outside groups must be ignored, got %+v, %v", got, err)
	}
}

func TestParseWebhookAllowsSubgroupRepo(t *testing.T) {
	g := newTestPlatform(t, "https://gitlab.example.com")
	body := strings.ReplaceAll(mrTicked, "top-group/app", "top-group/sub/deep/app")
	r := webhookRequest("Merge Request Hook", "s3cret", body)
	got, err := g.ParseWebhook(r, []byte(body))
	if err != nil || got == nil {
		t.Fatalf("subgroup repo must be allowed, got %+v, %v", got, err)
	}
}

func TestParseWebhookNoGroupsAllowsAll(t *testing.T) {
	cfg := testConfig("https://gitlab.example.com")
	cfg.Discovery.Groups = nil
	g, err := New(cfg, slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatal(err)
	}
	body := strings.ReplaceAll(mrTicked, "top-group/app", "anything/app")
	r := webhookRequest("Merge Request Hook", "s3cret", body)
	got, err := g.ParseWebhook(r, []byte(body))
	if err != nil || got == nil {
		t.Fatalf("empty groups must allow all repos, got %+v, %v", got, err)
	}
}

func TestParseWebhookAuthorIdentifiesMR(t *testing.T) {
	var userLookups atomic.Int32
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v4/users/42", func(w http.ResponseWriter, _ *http.Request) {
		userLookups.Add(1)
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"id": 42, "username": "renovate-bot"}`)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	cfg := testConfig(srv.URL)
	cfg.MRFilter = config.MRFilter{
		SourceBranchPrefixes: []string{"renovate/"},
		Authors:              []string{"renovate-bot"},
	}
	g, err := New(cfg, slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatal(err)
	}

	for i := range 2 { // second call must hit the cache
		r := webhookRequest("Merge Request Hook", "s3cret", mrAuthorTicked)
		got, err := g.ParseWebhook(r, []byte(mrAuthorTicked))
		if err != nil || got == nil {
			t.Fatalf("call %d: author-matched MR must trigger, got %+v, %v", i, got, err)
		}
	}
	if n := userLookups.Load(); n != 1 {
		t.Fatalf("user lookups = %d, want 1 (cached)", n)
	}
}

func TestParseWebhookAuthorMismatchIgnored(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v4/users/42", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"id": 42, "username": "some-human"}`)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	cfg := testConfig(srv.URL)
	cfg.MRFilter = config.MRFilter{
		SourceBranchPrefixes: []string{"renovate/"},
		Authors:              []string{"renovate-bot"},
	}
	g, err := New(cfg, slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatal(err)
	}
	r := webhookRequest("Merge Request Hook", "s3cret", mrAuthorTicked)
	got, err := g.ParseWebhook(r, []byte(mrAuthorTicked))
	if err != nil || got != nil {
		t.Fatalf("non-renovate author must not trigger, got %+v, %v", got, err)
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
}
