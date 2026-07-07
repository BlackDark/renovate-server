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
	t.Helper()
	st := store.NewMemory()
	reg := prometheus.NewRegistry()
	m := metrics.New(reg, st)
	enq := &fakeEnqueuer{}
	s := New([]platform.Platform{p}, enq, st, reg, m, slog.New(slog.DiscardHandler))
	return s, enq, st
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
