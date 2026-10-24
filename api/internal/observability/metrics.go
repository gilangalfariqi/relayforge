// Package observability configures metrics and tracing for the control plane.
package observability

import (
	"net/http"
	"strconv"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

// Metrics is the API's Prometheus registry and instrument set.
type Metrics struct {
	Registry         *prometheus.Registry
	Requests         *prometheus.CounterVec
	Duration         *prometheus.HistogramVec
	InFlight         prometheus.Gauge
	DBUp             prometheus.Gauge
	RateLimitDenied  *prometheus.CounterVec
	RetentionDeleted *prometheus.CounterVec
}

// NewMetrics builds an isolated registry. The default Go and process
// collectors are registered so runtime health is observable.
func NewMetrics(version, commit string) *Metrics {
	reg := prometheus.NewRegistry()
	reg.MustRegister(collectors.NewGoCollector())
	reg.MustRegister(collectors.NewProcessCollector(collectors.ProcessCollectorOpts{}))

	m := &Metrics{
		Registry: reg,
		Requests: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "relayforge_http_requests_total",
			Help: "HTTP requests by route, method, and status class.",
		}, []string{"route", "method", "status"}),
		Duration: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Name:    "relayforge_http_request_duration_seconds",
			Help:    "HTTP request duration in seconds.",
			Buckets: []float64{0.005, 0.01, 0.025, 0.05, 0.1, 0.15, 0.25, 0.4, 0.75, 1, 2.5},
		}, []string{"route", "method"}),
		InFlight: prometheus.NewGauge(prometheus.GaugeOpts{
			Name: "relayforge_http_in_flight_requests",
			Help: "Requests currently being served.",
		}),
		DBUp: prometheus.NewGauge(prometheus.GaugeOpts{
			Name: "relayforge_db_up",
			Help: "1 when the last readiness database check succeeded.",
		}),
		RateLimitDenied: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "relayforge_rate_limit_denied_total",
			Help: "Requests denied by fixed-window rate limiting.",
		}, []string{"scope"}),
		RetentionDeleted: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "relayforge_retention_deleted_rows_total",
			Help: "Rows deleted by the bounded retention cleanup worker.",
		}, []string{"table"}),
	}
	reg.MustRegister(m.Requests, m.Duration, m.InFlight, m.DBUp, m.RateLimitDenied, m.RetentionDeleted)

	build := prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "relayforge_build_info",
		Help: "Build identity; the value is always 1.",
	}, []string{"version", "commit"})
	build.WithLabelValues(version, commit).Set(1)
	reg.MustRegister(build)

	return m
}

// Handler serves the Prometheus exposition format.
func (m *Metrics) Handler() http.Handler {
	return promhttp.HandlerFor(m.Registry, promhttp.HandlerOpts{})
}

// Observe records one completed request. route must be the templated route
// (for example /api/v1/events), never the raw path, to bound cardinality.
func (m *Metrics) Observe(route, method string, status int, d time.Duration) {
	if route == "" {
		route = "unmatched"
	}
	m.Requests.WithLabelValues(route, method, strconv.Itoa(status)).Inc()
	m.Duration.WithLabelValues(route, method).Observe(d.Seconds())
}
