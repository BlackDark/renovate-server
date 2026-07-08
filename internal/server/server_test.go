package server

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/BlackDark/renovate-server/internal/config"
	"github.com/BlackDark/renovate-server/internal/history"
	"github.com/BlackDark/renovate-server/internal/metrics"
	"github.com/BlackDark/renovate-server/internal/platform"
	"github.com/BlackDark/renovate-server/internal/store"
)

// fakePlatform returns canned ParseWebhook results.
type fakePlatform struct {
	name string
	path string
	ev   *platform.Event
	err  error
}

func (f *fakePlatform) Name() string              { return f.name }
func (f *fakePlatform) WebhookPath() string       { return f.path }
func (f *fakePlatform) Schedule() config.Schedule { return config.Schedule{} }
func (f *fakePlatform) DiscoverRepos(context.Context) ([]platform.Repo, error) {
	return nil, nil
}
func (f *fakePlatform) ParseWebhook(*http.Request, []byte) (*platform.Event, error) {
	return f.ev, f.err
}

func (f *fakePlatform) AllowsRepo(fullName string) bool {
	return strings.HasPrefix(fullName, "g/")
}

type fakeEnqueuer struct {
	mu     sync.Mutex
	events []platform.Event
}

func (f *fakeEnqueuer) Enqueue(ev platform.Event) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.events = append(f.events, ev)
}

func testServer(t *testing.T, p platform.Platform) (*Server, *fakeEnqueuer, store.Store) {
	s, enq, st, _ := testServerWithHistory(t, p)
	return s, enq, st
}

func testServerWithHistory(t *testing.T, p platform.Platform) (*Server, *fakeEnqueuer, store.Store, *history.History) {
	t.Helper()
	st := store.NewMemory()
	reg := prometheus.NewRegistry()
	m := metrics.New(reg, st)
	enq := &fakeEnqueuer{}
	hist := history.New(10)
	s := New([]platform.Platform{p}, enq, st, hist, reg, m, slog.New(slog.DiscardHandler))
	return s, enq, st, hist
}

func TestRunsEndpoint(t *testing.T) {
	s, _, _, hist := testServerWithHistory(t, &fakePlatform{name: "gl", path: "/webhook/gitlab"})
	hist.Record(history.Entry{Repo: "gl:g/a", Result: "success", Reason: "push", Executor: "ci"})
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, httptest.NewRequest("GET", "/api/v1/runs", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("runs = %d", rec.Code)
	}
	var body struct {
		Runs []history.Entry `json:"runs"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if len(body.Runs) != 1 || body.Runs[0].Repo != "gl:g/a" || body.Runs[0].Result != "success" {
		t.Fatalf("body = %s", rec.Body.String())
	}
}

func TestWebhookAccepted(t *testing.T) {
	ev := &platform.Event{
		Repo:   platform.Repo{Platform: "gl", FullName: "g/a"},
		Reason: platform.ReasonPush,
	}
	s, enq, _ := testServer(t, &fakePlatform{name: "gl", path: "/webhook/gitlab", ev: ev})
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, httptest.NewRequest("POST", "/webhook/gitlab", strings.NewReader("{}")))
	if rec.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want 202", rec.Code)
	}
	if len(enq.events) != 1 || enq.events[0] != *ev {
		t.Fatalf("enqueued = %+v", enq.events)
	}
}

func TestWebhookIgnored(t *testing.T) {
	s, enq, _ := testServer(t, &fakePlatform{name: "gl", path: "/webhook/gitlab"})
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, httptest.NewRequest("POST", "/webhook/gitlab", strings.NewReader("{}")))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if len(enq.events) != 0 {
		t.Fatalf("nothing should be enqueued, got %+v", enq.events)
	}
}

func TestWebhookUnauthorized(t *testing.T) {
	s, _, _ := testServer(t, &fakePlatform{name: "gl", path: "/webhook/gitlab", err: platform.ErrUnauthorized})
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, httptest.NewRequest("POST", "/webhook/gitlab", strings.NewReader("{}")))
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", rec.Code)
	}
}

func TestWebhookMalformed(t *testing.T) {
	s, _, _ := testServer(t, &fakePlatform{name: "gl", path: "/webhook/gitlab", err: &json.SyntaxError{}})
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, httptest.NewRequest("POST", "/webhook/gitlab", strings.NewReader("{}")))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rec.Code)
	}
}

func TestHealthAndReady(t *testing.T) {
	s, _, _ := testServer(t, &fakePlatform{name: "gl", path: "/webhook/gitlab"})
	h := s.Handler()

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/healthz", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("healthz = %d", rec.Code)
	}

	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/readyz", nil))
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("readyz before ready = %d, want 503", rec.Code)
	}
	s.SetReady(true)
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/readyz", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("readyz after ready = %d, want 200", rec.Code)
	}
}

func TestStatusEndpoint(t *testing.T) {
	s, _, st := testServer(t, &fakePlatform{name: "gl", path: "/webhook/gitlab"})
	st.Queue("gl:g/a", "push")
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, httptest.NewRequest("GET", "/api/v1/status", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	var body struct {
		Repos map[string]store.RepoStatus `json:"repos"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if body.Repos["gl:g/a"].State != store.StateQueued {
		t.Fatalf("body = %s", rec.Body.String())
	}
}

func triggerRequest(token, body string) *http.Request {
	r := httptest.NewRequest("POST", "/api/v1/trigger", strings.NewReader(body))
	if token != "" {
		r.Header.Set("Authorization", "Bearer "+token)
	}
	return r
}

func TestTriggerEndpoint(t *testing.T) {
	st := store.NewMemory()
	reg := prometheus.NewRegistry()
	m := metrics.New(reg, st)
	enq := &fakeEnqueuer{}
	p := &fakePlatform{name: "gl", path: "/webhook/gitlab"}
	s := New([]platform.Platform{p}, enq, st, history.New(10), reg, m, slog.New(slog.DiscardHandler))
	s.SetAPIToken("sekret")
	h := s.Handler()

	cases := []struct {
		name     string
		token    string
		body     string
		wantCode int
		wantRuns int
	}{
		{"accepted", "sekret", `{"platform":"gl","repo":"g/app"}`, http.StatusAccepted, 1},
		{"wrong token", "nope", `{"platform":"gl","repo":"g/app"}`, http.StatusUnauthorized, 0},
		{"missing token", "", `{"platform":"gl","repo":"g/app"}`, http.StatusUnauthorized, 0},
		{"unknown platform", "sekret", `{"platform":"gh","repo":"g/app"}`, http.StatusBadRequest, 0},
		{"repo outside groups", "sekret", `{"platform":"gl","repo":"other/app"}`, http.StatusForbidden, 0},
		{"malformed body", "sekret", `{not json`, http.StatusBadRequest, 0},
		{"missing fields", "sekret", `{}`, http.StatusBadRequest, 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			before := len(enq.events)
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, triggerRequest(tc.token, tc.body))
			if rec.Code != tc.wantCode {
				t.Fatalf("status = %d, want %d (%s)", rec.Code, tc.wantCode, rec.Body.String())
			}
			if got := len(enq.events) - before; got != tc.wantRuns {
				t.Fatalf("enqueued = %d, want %d", got, tc.wantRuns)
			}
		})
	}
	if enq.events[0].Reason != platform.ReasonManual || enq.events[0].Repo.Key() != "gl:g/app" {
		t.Fatalf("event = %+v", enq.events[0])
	}
}

func TestTriggerEndpointAbsentWithoutToken(t *testing.T) {
	s, _, _ := testServer(t, &fakePlatform{name: "gl", path: "/webhook/gitlab"})
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, triggerRequest("anything", `{"platform":"gl","repo":"g/app"}`))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404 when no apiToken configured", rec.Code)
	}
}

func TestMetricsEndpoint(t *testing.T) {
	s, _, _ := testServer(t, &fakePlatform{name: "gl", path: "/webhook/gitlab"})
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, httptest.NewRequest("GET", "/metrics", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("metrics = %d", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "renovate_server_repos_active") {
		t.Error("gauge missing from /metrics output")
	}
}
