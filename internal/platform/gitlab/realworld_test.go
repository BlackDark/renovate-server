package gitlab

// Regression tests built from sanitized real-world payloads of a private
// GitLab instance (renovate 43.x): the MR rebase checkbox carries no HTML
// marker, identification relies on the trailing renovate-debug comment;
// dashboard issue checkboxes carry action markers but no debug comment.

import (
	"encoding/json"
	"os"
	"strings"
	"testing"

	"github.com/BlackDark/renovate-server/internal/platform"
)

func loadFixture(t *testing.T, name string) string {
	t.Helper()
	raw, err := os.ReadFile("testdata/" + name)
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}

func mrPayload(t *testing.T, sourceBranch, previous, current string) string {
	t.Helper()
	payload, err := json.Marshal(map[string]any{
		"object_kind": "merge_request",
		"project":     map[string]any{"path_with_namespace": "top-group/app", "default_branch": "main"},
		"object_attributes": map[string]any{
			"iid": 6, "action": "update",
			"source_branch": sourceBranch,
			"description":   current,
		},
		"changes": map[string]any{
			"description": map[string]any{"previous": previous, "current": current},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	return string(payload)
}

func issuePayload(t *testing.T, title, previous, current string) string {
	t.Helper()
	payload, err := json.Marshal(map[string]any{
		"object_kind": "issue",
		"project":     map[string]any{"path_with_namespace": "top-group/app", "default_branch": "main"},
		"object_attributes": map[string]any{
			"iid": 1, "action": "update",
			"title":       title,
			"description": current,
		},
		"changes": map[string]any{
			"description": map[string]any{"previous": previous, "current": current},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	return string(payload)
}

func parse(t *testing.T, g *GitLab, eventType, body string) *platform.Event {
	t.Helper()
	r := webhookRequest(eventType, "s3cret", body)
	ev, err := g.ParseWebhook(r, []byte(body))
	if err != nil {
		t.Fatalf("ParseWebhook: %v", err)
	}
	return ev
}

func TestRealWorldMRRebaseTick(t *testing.T) {
	g := newTestPlatform(t, "https://gitlab.example.com")
	previous := loadFixture(t, "mr-description.md")
	current := strings.Replace(previous,
		"- [ ] If you want to rebase/retry this MR, check this box",
		"- [x] If you want to rebase/retry this MR, check this box", 1)
	if previous == current {
		t.Fatal("fixture must contain the rebase checkbox")
	}

	// Non-renovate branch name: identification must come from the
	// renovate-debug comment alone.
	body := mrPayload(t, "chore/some-branch", previous, current)
	ev := parse(t, g, "Merge Request Hook", body)
	if ev == nil || ev.Reason != platform.ReasonMergeRequest {
		t.Fatalf("real-world rebase tick must trigger, got %+v", ev)
	}
}

func TestRealWorldMRBodyUpdateNoTick(t *testing.T) {
	g := newTestPlatform(t, "https://gitlab.example.com")
	desc := loadFixture(t, "mr-description.md")
	// Renovate refreshing release notes: description changes, no new tick.
	body := mrPayload(t, "chore/some-branch", desc, desc+"\nupdated release notes")
	if ev := parse(t, g, "Merge Request Hook", body); ev != nil {
		t.Fatalf("body update without tick must not trigger, got %+v", ev)
	}
}

func TestRealWorldDashboardRebaseAllTick(t *testing.T) {
	g := newTestPlatform(t, "https://gitlab.example.com")
	previous := loadFixture(t, "dashboard-issue.md")
	current := strings.Replace(previous,
		"- [ ] <!-- rebase-all-open-prs -->",
		"- [x] <!-- rebase-all-open-prs -->", 1)
	if previous == current {
		t.Fatal("fixture must contain the rebase-all checkbox")
	}

	body := issuePayload(t, "Dependency Dashboard", previous, current)
	ev := parse(t, g, "Issue Hook", body)
	if ev == nil || ev.Reason != platform.ReasonIssue {
		t.Fatalf("dashboard rebase-all tick must trigger, got %+v", ev)
	}
}

func TestRealWorldDashboardRenamedTitleMarkerFallback(t *testing.T) {
	// Dashboard renamed in renovate config but not in server config: the
	// marker-carrying checkboxes still identify it.
	g := newTestPlatform(t, "https://gitlab.example.com")
	previous := loadFixture(t, "dashboard-issue.md")
	current := strings.Replace(previous,
		"- [ ] <!-- rebase-branch=renovate/pnpm-11.x -->",
		"- [x] <!-- rebase-branch=renovate/pnpm-11.x -->", 1)

	body := issuePayload(t, "Custom Dashboard Title", previous, current)
	ev := parse(t, g, "Issue Hook", body)
	if ev == nil {
		t.Fatal("marker-carrying dashboard with unconfigured title must trigger")
	}
}

func TestRealWorldIssueWrongTitlePlainCheckboxIgnored(t *testing.T) {
	g := newTestPlatform(t, "https://gitlab.example.com")
	body := issuePayload(t, "Team TODO list", "- [ ] buy coffee", "- [x] buy coffee")
	if ev := parse(t, g, "Issue Hook", body); ev != nil {
		t.Fatalf("human todo issue must not trigger, got %+v", ev)
	}
}
