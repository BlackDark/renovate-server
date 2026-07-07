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
		Name:      "gh",
		Type:      config.PlatformGitHub,
		BaseURL:   baseURL,
		Token:     "ghp_test",
		BotEmail:  "renovate@example.com",
		Webhook:   config.Webhook{Path: "/webhook/github", Secret: "s3cret"},
		Events:    []string{"merge_request", "issue", "push"},
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

func webhookRequest(eventType, secret, body string) *http.Request {
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
