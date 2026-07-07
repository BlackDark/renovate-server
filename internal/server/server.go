// Package server exposes the HTTP surface: webhook receivers per platform
// and operational endpoints (health, readiness, metrics, status).
package server

import (
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"sync/atomic"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"

	"github.com/BlackDark/renovate-server/internal/metrics"
	"github.com/BlackDark/renovate-server/internal/platform"
	"github.com/BlackDark/renovate-server/internal/store"
)

const maxWebhookBody = 1 << 20 // 1 MiB

// Enqueuer is the dispatcher surface the server needs.
type Enqueuer interface {
	Enqueue(ev platform.Event)
}

type Server struct {
	mux   *http.ServeMux
	ready atomic.Bool
	log   *slog.Logger
}

func New(platforms []platform.Platform, enq Enqueuer, st store.Store,
	reg *prometheus.Registry, m *metrics.Metrics, log *slog.Logger) *Server {
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
	return s
}

func (s *Server) Handler() http.Handler { return s.mux }

// SetReady flips the readiness probe; main calls it after startup completes.
func (s *Server) SetReady(ready bool) { s.ready.Store(ready) }

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
