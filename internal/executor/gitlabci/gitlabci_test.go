package gitlabci

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	gogitlab "gitlab.com/gitlab-org/api/client-go"

	"github.com/BlackDark/renovate-server/internal/config"
	"github.com/BlackDark/renovate-server/internal/executor"
	"github.com/BlackDark/renovate-server/internal/platform"
)

type pipelineServer struct {
	*httptest.Server
	triggered       atomic.Int32
	polls           atomic.Int32
	finalStatus     string
	pollsUntilFinal int32
	gotVars         map[string]string
	gotRef          string
}

func newPipelineServer(t *testing.T, finalStatus string, pollsUntilFinal int32) *pipelineServer {
	t.Helper()
	ps := &pipelineServer{finalStatus: finalStatus, pollsUntilFinal: pollsUntilFinal}
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v4/", func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/trigger/pipeline"):
			ps.triggered.Add(1)
			var payload struct {
				Ref       string            `json:"ref"`
				Token     string            `json:"token"`
				Variables map[string]string `json:"variables"`
			}
			if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
				t.Errorf("decode trigger payload: %v", err)
			}
			ps.gotRef = payload.Ref
			ps.gotVars = payload.Variables
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusCreated)
			fmt.Fprint(w, `{"id": 42, "status": "created"}`)
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/pipelines/42"):
			n := ps.polls.Add(1)
			status := "running"
			if n >= ps.pollsUntilFinal {
				status = ps.finalStatus
			}
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]any{"id": 42, "status": status})
		default:
			t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	})
	ps.Server = httptest.NewServer(mux)
	t.Cleanup(ps.Close)
	return ps
}

type fakeHandles struct {
	mu      sync.Mutex
	saved   map[string]string
	deleted []string
}

func newFakeHandles() *fakeHandles {
	return &fakeHandles{saved: map[string]string{}}
}

func (f *fakeHandles) SaveRunHandle(key, data string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.saved[key] = data
}

func (f *fakeHandles) LoadRunHandles() map[string]string {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := map[string]string{}
	for k, v := range f.saved {
		out[k] = v
	}
	return out
}

func (f *fakeHandles) DeleteRunHandle(key string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.saved, key)
	f.deleted = append(f.deleted, key)
}

func newExecutor(t *testing.T, baseURL string) *Executor {
	return newExecutorWithHandles(t, baseURL, nil)
}

func newExecutorWithHandles(t *testing.T, baseURL string, handles HandleStore) *Executor {
	t.Helper()
	client, err := gogitlab.NewClient("tok", gogitlab.WithBaseURL(baseURL))
	if err != nil {
		t.Fatal(err)
	}
	e, err := newFromClient(t, client, handles)
	if err != nil {
		t.Fatal(err)
	}
	return e
}

func newFromClient(t *testing.T, client *gogitlab.Client, handles HandleStore) (*Executor, error) {
	t.Helper()
	return New(config.Executor{
		Name:         "ci",
		Type:         config.ExecutorGitLabPipeline,
		Project:      "infra/renovate-runner",
		Ref:          "main",
		TriggerToken: "trigger-tok",
		Variables: map[string]string{
			"RENOVATE_REPO":  "{{ .Repo }}",
			"TRIGGER_REASON": "{{ .Reason }}",
			"STATIC_VAR":     "fixed",
		},
		PollInterval: 5 * time.Millisecond,
	}, client, handles, slog.New(slog.DiscardHandler))
}

func spec() executor.RunSpec {
	return executor.RunSpec{
		Repo:   platform.Repo{Platform: "gl", FullName: "top-group/app"},
		Reason: platform.ReasonMergeRequest,
	}
}

func TestRunSuccess(t *testing.T) {
	srv := newPipelineServer(t, "success", 3)
	e := newExecutor(t, srv.URL)
	if err := e.Run(t.Context(), spec()); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if srv.triggered.Load() != 1 {
		t.Errorf("triggered %d times", srv.triggered.Load())
	}
	if srv.gotRef != "main" {
		t.Errorf("ref = %q", srv.gotRef)
	}
	if srv.gotVars["RENOVATE_REPO"] != "top-group/app" {
		t.Errorf("RENOVATE_REPO = %q, want top-group/app", srv.gotVars["RENOVATE_REPO"])
	}
	if srv.gotVars["TRIGGER_REASON"] != "merge_request" {
		t.Errorf("TRIGGER_REASON = %q", srv.gotVars["TRIGGER_REASON"])
	}
	if srv.gotVars["STATIC_VAR"] != "fixed" {
		t.Errorf("STATIC_VAR = %q", srv.gotVars["STATIC_VAR"])
	}
}

func TestRunPipelineFailed(t *testing.T) {
	srv := newPipelineServer(t, "failed", 2)
	e := newExecutor(t, srv.URL)
	err := e.Run(t.Context(), spec())
	if err == nil {
		t.Fatal("want error for failed pipeline")
	}
}

func TestRunContextCancelled(t *testing.T) {
	srv := newPipelineServer(t, "success", 1000) // stays running
	e := newExecutor(t, srv.URL)
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Millisecond)
	defer cancel()
	err := e.Run(ctx, spec())
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("want DeadlineExceeded, got %v", err)
	}
}

func TestInvalidTemplateRejectedAtConstruction(t *testing.T) {
	client, err := gogitlab.NewClient("tok")
	if err != nil {
		t.Fatal(err)
	}
	_, err = New(config.Executor{
		Name: "ci", Project: "p", TriggerToken: "t", Ref: "main",
		Variables:    map[string]string{"BAD": "{{ .Nope"},
		PollInterval: time.Second,
	}, client, nil, slog.New(slog.DiscardHandler))
	if err == nil {
		t.Fatal("want template parse error at construction")
	}
}

func TestRunPersistsAndClearsHandle(t *testing.T) {
	srv := newPipelineServer(t, "success", 2)
	handles := newFakeHandles()
	var sawHandle atomic.Bool
	e := newExecutorWithHandles(t, srv.URL, handles)

	// capture handle state while the run is in flight
	go func() {
		deadline := time.Now().Add(5 * time.Second)
		for time.Now().Before(deadline) {
			if len(handles.LoadRunHandles()) == 1 {
				sawHandle.Store(true)
				return
			}
			time.Sleep(time.Millisecond)
		}
	}()

	if err := e.Run(t.Context(), spec()); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !sawHandle.Load() {
		t.Error("handle was never persisted during the run")
	}
	if got := handles.LoadRunHandles(); len(got) != 0 {
		t.Fatalf("handle not cleared after run: %v", got)
	}
	if len(handles.deleted) == 0 || handles.deleted[0] != "gl:top-group/app" {
		t.Fatalf("deleted = %v", handles.deleted)
	}
}

func TestAdoptRunningResumesPipeline(t *testing.T) {
	srv := newPipelineServer(t, "success", 2)
	handles := newFakeHandles()
	handles.SaveRunHandle("gl:top-group/app", `{"executor":"ci","platform":"gl","repo":"top-group/app","project":"infra/renovate-runner","pipelineID":42}`)
	e := newExecutorWithHandles(t, srv.URL, handles)

	adopted, err := e.AdoptRunning(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if len(adopted) != 1 {
		t.Fatalf("adopted = %d, want 1", len(adopted))
	}
	if adopted[0].Repo != (platform.Repo{Platform: "gl", FullName: "top-group/app"}) {
		t.Fatalf("adopted repo = %+v", adopted[0].Repo)
	}
	if err := adopted[0].Wait(t.Context()); err != nil {
		t.Fatalf("Wait: %v", err)
	}
	if srv.triggered.Load() != 0 {
		t.Fatal("adoption must not trigger a new pipeline")
	}
	if got := handles.LoadRunHandles(); len(got) != 0 {
		t.Fatalf("handle not cleared after adopted run: %v", got)
	}
}

func TestAdoptRunningIgnoresForeignAndCorruptHandles(t *testing.T) {
	srv := newPipelineServer(t, "success", 1)
	handles := newFakeHandles()
	handles.SaveRunHandle("gl:other/app", `{"executor":"other-executor","platform":"gl","repo":"other/app","project":"p","pipelineID":7}`)
	handles.SaveRunHandle("gl:broken/app", `{not json`)
	e := newExecutorWithHandles(t, srv.URL, handles)

	adopted, err := e.AdoptRunning(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if len(adopted) != 0 {
		t.Fatalf("adopted = %d, want 0", len(adopted))
	}
	// foreign handle untouched, corrupt handle cleaned up
	got := handles.LoadRunHandles()
	if _, ok := got["gl:other/app"]; !ok {
		t.Error("foreign handle must be preserved")
	}
	if _, ok := got["gl:broken/app"]; ok {
		t.Error("corrupt handle must be deleted")
	}
}
