// Package metrics exposes Prometheus instrumentation for the server.
package metrics

import (
	"github.com/prometheus/client_golang/prometheus"

	"github.com/BlackDark/renovate-server/internal/store"
)

// Metrics bundles all Prometheus collectors of the server.
type Metrics struct {
	webhookEvents *prometheus.CounterVec
	runsStarted   *prometheus.CounterVec
	runsFinished  *prometheus.CounterVec
	runDuration   *prometheus.HistogramVec
}

// New registers all metrics on reg. The store feeds the repo-state gauge.
func New(reg *prometheus.Registry, st store.Store) *Metrics {
	m := &Metrics{
		webhookEvents: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "renovate_server_webhook_events_total",
			Help: "Webhook events received, by platform and outcome.",
		}, []string{"platform", "outcome"}),
		runsStarted: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "renovate_server_runs_started_total",
			Help: "Renovate runs started, by executor.",
		}, []string{"executor"}),
		runsFinished: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "renovate_server_runs_finished_total",
			Help: "Renovate runs finished, by executor and result.",
		}, []string{"executor", "result"}),
		runDuration: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Name:    "renovate_server_run_duration_seconds",
			Help:    "Duration of renovate runs, by executor.",
			Buckets: prometheus.ExponentialBuckets(10, 2, 10), // 10s .. ~85m
		}, []string{"executor"}),
	}
	reg.MustRegister(m.webhookEvents, m.runsStarted, m.runsFinished, m.runDuration)
	reg.MustRegister(prometheus.NewGaugeFunc(prometheus.GaugeOpts{
		Name: "renovate_server_repos_active",
		Help: "Repos currently queued or running.",
	}, func() float64 { return float64(len(st.Snapshot())) }))
	return m
}

// WebhookEvent counts one webhook request by platform and outcome
// (accepted|ignored|unauthorized|invalid).
func (m *Metrics) WebhookEvent(platformName, outcome string) {
	m.webhookEvents.WithLabelValues(platformName, outcome).Inc()
}

// RunStarted counts a started run; implements dispatch.Metrics.
func (m *Metrics) RunStarted(executorName string) {
	m.runsStarted.WithLabelValues(executorName).Inc()
}

// RunFinished records a finished run's result (success|failure|timeout)
// and duration; implements dispatch.Metrics.
func (m *Metrics) RunFinished(executorName, result string, seconds float64) {
	m.runsFinished.WithLabelValues(executorName, result).Inc()
	m.runDuration.WithLabelValues(executorName).Observe(seconds)
}
