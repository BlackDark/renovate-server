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

func newExecutor(t *testing.T, baseURL string) *Executor {
	t.Helper()
	client, err := gogitlab.NewClient("tok", gogitlab.WithBaseURL(baseURL))
	if err != nil {
		t.Fatal(err)
	}
	e, err := New(config.Executor{
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
	}, client, slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatal(err)
	}
	return e
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
	}, client, slog.New(slog.DiscardHandler))
	if err == nil {
		t.Fatal("want template parse error at construction")
	}
}
