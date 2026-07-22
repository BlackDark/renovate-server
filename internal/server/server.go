// Package server exposes the HTTP surface: webhook receivers per platform
// and operational endpoints (health, readiness, metrics, status).
package server

import (
	"crypto/subtle"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"sync/atomic"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"

	"github.com/BlackDark/renovate-server/internal/history"
	"github.com/BlackDark/renovate-server/internal/metrics"
	"github.com/BlackDark/renovate-server/internal/platform"
	"github.com/BlackDark/renovate-server/internal/store"
)

// Renovate MR descriptions with release notes easily reach hundreds of KiB
// and appear up to three times per payload (description + change diff).
const maxWebhookBody = 5 << 20 // 5 MiB

// Enqueuer is the dispatcher surface the server needs.
type Enqueuer interface {
	Enqueue(ev platform.Event)
}

// Server routes webhook and operational HTTP endpoints.
type Server struct {
	mux      *http.ServeMux
	ready    atomic.Bool
	apiToken []byte
	log      *slog.Logger
}

// New builds the HTTP surface: one webhook route per platform plus
// healthz, readyz, metrics, run history and the status API.
func New(platforms []platform.Platform, enq Enqueuer, st store.Store,
	hist *history.History, reg *prometheus.Registry, m *metrics.Metrics, log *slog.Logger) *Server {
	s := &Server{mux: http.NewServeMux(), log: log}

	for _, p := range platforms {
		s.mux.HandleFunc("POST "+p.WebhookPath(), s.webhookHandler(p, enq, m))
	}

	s.mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	s.mux.HandleFunc("GET /readyz", func(w http.ResponseWriter, _ *http.Request) {
		if !s.ready.Load() {
			http.Error(w, "not ready", http.StatusServiceUnavailable)
			return
		}
		w.WriteHeader(http.StatusOK)
	})
	s.mux.Handle("GET /metrics", promhttp.HandlerFor(reg, promhttp.HandlerOpts{}))
	s.mux.HandleFunc("GET /api/v1/status", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"repos": st.Snapshot()})
	})
	s.mux.HandleFunc("GET /api/v1/runs", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"runs": hist.Entries()})
	})
	s.mux.HandleFunc("POST /api/v1/trigger", s.triggerHandler(platforms, enq))
	return s
}

// Handler returns the root HTTP handler.
func (s *Server) Handler() http.Handler { return s.mux }

// SetReady flips the readiness probe; main calls it after startup completes.
func (s *Server) SetReady(ready bool) { s.ready.Store(ready) }

// SetAPIToken enables the authenticated admin API (/api/v1/trigger).
// Without a token the endpoint responds 404.
func (s *Server) SetAPIToken(token string) {
	if token != "" {
		s.apiToken = []byte(token)
	}
}

// triggerHandler enqueues a manual run for a repo. Requires the configured
// bearer token; the repo must be inside its platform's configured groups.
func (s *Server) triggerHandler(platforms []platform.Platform, enq Enqueuer) http.HandlerFunc {
	byName := make(map[string]platform.Platform, len(platforms))
	for _, p := range platforms {
		byName[p.Name()] = p
	}
	return func(w http.ResponseWriter, r *http.Request) {
		if len(s.apiToken) == 0 {
			http.NotFound(w, r)
			return
		}
		token, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
		if !ok || subtle.ConstantTimeCompare([]byte(token), s.apiToken) != 1 {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}

		var req struct {
			Platform string `json:"platform"`
			Repo     string `json:"repo"`
		}
		body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 4096))
		if err != nil || json.Unmarshal(body, &req) != nil || req.Platform == "" || req.Repo == "" {
			http.Error(w, `expected {"platform": "...", "repo": "..."}`, http.StatusBadRequest)
			return
		}
		p, ok := byName[req.Platform]
		if !ok {
			http.Error(w, "unknown platform "+req.Platform, http.StatusBadRequest)
			return
		}
		if !p.AllowsRepo(req.Repo) {
			http.Error(w, "repo outside configured groups", http.StatusForbidden)
			return
		}

		s.log.Info("manual trigger", "platform", req.Platform, "repo", req.Repo)
		enq.Enqueue(platform.Event{
			Repo:   platform.Repo{Platform: req.Platform, FullName: req.Repo},
			Reason: platform.ReasonManual,
		})
		w.WriteHeader(http.StatusAccepted)
	}
}

func (s *Server) webhookHandler(p platform.Platform, enq Enqueuer, m *metrics.Metrics) http.HandlerFunc {
	log := s.log.With("platform", p.Name())
	return func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxWebhookBody))
		if err != nil {
			m.WebhookEvent(p.Name(), "invalid")
			http.Error(w, "failed to read body", http.StatusBadRequest)
			return
		}

		ev, err := p.ParseWebhook(r, body)
		switch {
		case errors.Is(err, platform.ErrUnauthorized):
			m.WebhookEvent(p.Name(), "unauthorized")
			log.Warn("webhook authentication failed", "remote", r.RemoteAddr)
			http.Error(w, "unauthorized", http.StatusUnauthorized)
		case err != nil:
			m.WebhookEvent(p.Name(), "invalid")
			log.Warn("webhook payload invalid", "error", err)
			http.Error(w, "invalid payload", http.StatusBadRequest)
		case ev == nil:
			m.WebhookEvent(p.Name(), "ignored")
			w.WriteHeader(http.StatusOK)
		default:
			m.WebhookEvent(p.Name(), "accepted")
			enq.Enqueue(*ev)
			w.WriteHeader(http.StatusAccepted)
		}
	}
}
