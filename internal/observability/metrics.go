package observability

import (
	"net/http"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

type Metrics struct {
	registry               *prometheus.Registry
	ActiveSandboxes        *prometheus.GaugeVec
	LifecycleDuration      *prometheus.HistogramVec
	CloneDuration          prometheus.Histogram
	CloneFailures          prometheus.Counter
	IdleStops              prometheus.Counter
	RetainedPVCs           prometheus.Gauge
	ProvisionedGiB         prometheus.Gauge
	ActiveConnections      *prometheus.GaugeVec
	ActiveJobs             prometheus.Gauge
	ReconciliationFailures prometheus.Counter
	TokenRefreshFailures   prometheus.Counter
}

func NewMetrics(service string) *Metrics {
	registry := prometheus.NewRegistry()
	metrics := newMetrics(service, registry)
	metrics.registry = registry
	return metrics
}

func NewRegisteredMetrics(service string, registerer prometheus.Registerer) *Metrics {
	return newMetrics(service, registerer)
}

func newMetrics(service string, registerer prometheus.Registerer) *Metrics {
	const namespace = "devsandbox"
	m := &Metrics{
		ActiveSandboxes:        prometheus.NewGaugeVec(prometheus.GaugeOpts{Namespace: namespace, Subsystem: service, Name: "active_sandboxes", Help: "Current sandboxes by template, profile, and state."}, []string{"template", "profile", "state"}),
		LifecycleDuration:      prometheus.NewHistogramVec(prometheus.HistogramOpts{Namespace: namespace, Subsystem: service, Name: "lifecycle_duration_seconds", Help: "Create and resume operation duration.", Buckets: prometheus.DefBuckets}, []string{"operation"}),
		CloneDuration:          prometheus.NewHistogram(prometheus.HistogramOpts{Namespace: namespace, Subsystem: service, Name: "clone_duration_seconds", Help: "Repository clone duration.", Buckets: prometheus.DefBuckets}),
		CloneFailures:          prometheus.NewCounter(prometheus.CounterOpts{Namespace: namespace, Subsystem: service, Name: "clone_failures_total", Help: "Repository clone failures."}),
		IdleStops:              prometheus.NewCounter(prometheus.CounterOpts{Namespace: namespace, Subsystem: service, Name: "idle_stops_total", Help: "Sandboxes stopped for idleness."}),
		RetainedPVCs:           prometheus.NewGauge(prometheus.GaugeOpts{Namespace: namespace, Subsystem: service, Name: "retained_pvcs", Help: "Retained workspace PVC count."}),
		ProvisionedGiB:         prometheus.NewGauge(prometheus.GaugeOpts{Namespace: namespace, Subsystem: service, Name: "provisioned_storage_gib", Help: "Provisioned workspace storage in GiB."}),
		ActiveConnections:      prometheus.NewGaugeVec(prometheus.GaugeOpts{Namespace: namespace, Subsystem: service, Name: "active_connections", Help: "Current interactive connections by type."}, []string{"type"}),
		ActiveJobs:             prometheus.NewGauge(prometheus.GaugeOpts{Namespace: namespace, Subsystem: service, Name: "active_managed_jobs", Help: "Current managed jobs."}),
		ReconciliationFailures: prometheus.NewCounter(prometheus.CounterOpts{Namespace: namespace, Subsystem: service, Name: "reconciliation_failures_total", Help: "Controller reconciliation failures."}),
		TokenRefreshFailures:   prometheus.NewCounter(prometheus.CounterOpts{Namespace: namespace, Subsystem: service, Name: "token_refresh_failures_total", Help: "Credential refresh failures."}),
	}
	registerer.MustRegister(m.ActiveSandboxes, m.LifecycleDuration, m.CloneDuration, m.CloneFailures, m.IdleStops,
		m.RetainedPVCs, m.ProvisionedGiB, m.ActiveConnections, m.ActiveJobs, m.ReconciliationFailures, m.TokenRefreshFailures)
	return m
}

func (m *Metrics) Handler() http.Handler {
	if m == nil || m.registry == nil {
		return http.NotFoundHandler()
	}
	return promhttp.HandlerFor(m.registry, promhttp.HandlerOpts{})
}
