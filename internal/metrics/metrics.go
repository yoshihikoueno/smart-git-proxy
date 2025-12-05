package metrics

import "github.com/prometheus/client_golang/prometheus"

type Metrics struct {
	RequestsTotal   *prometheus.CounterVec
	ResponsesTotal  *prometheus.CounterVec
	ErrorsTotal     *prometheus.CounterVec
	UpstreamLatency *prometheus.HistogramVec
	SyncTotal       *prometheus.CounterVec
}

// New creates metrics registered with the default prometheus registry.
func New() *Metrics {
	return NewWithRegistry(prometheus.DefaultRegisterer)
}

// NewUnregistered creates metrics without registering them (useful for tests).
func NewUnregistered() *Metrics {
	return NewWithRegistry(nil)
}

// NewWithRegistry creates metrics and registers them with the given registerer.
// Pass nil to skip registration.
func NewWithRegistry(reg prometheus.Registerer) *Metrics {
	m := &Metrics{
		RequestsTotal: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "smart_git_proxy_requests_total",
			Help: "requests received",
		}, []string{"repo", "kind", "source"}),
		ResponsesTotal: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "smart_git_proxy_responses_total",
			Help: "responses sent",
		}, []string{"repo", "kind", "status"}),
		ErrorsTotal: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "smart_git_proxy_errors_total",
			Help: "errors by repo/kind",
		}, []string{"repo", "kind"}),
		UpstreamLatency: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Name:    "smart_git_proxy_request_seconds",
			Help:    "request latency",
			Buckets: prometheus.DefBuckets,
		}, []string{"repo", "kind"}),
		SyncTotal: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "smart_git_proxy_sync_total",
			Help: "mirror sync operations",
		}, []string{"repo", "result"}),
	}

	if reg != nil {
		reg.MustRegister(
			m.RequestsTotal,
			m.ResponsesTotal,
			m.ErrorsTotal,
			m.UpstreamLatency,
			m.SyncTotal,
		)
	}
	return m
}
