package metrics

import (
	"net/http"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

var (
	SessionsCreated = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "telegram_mcp_sessions_created_total",
			Help: "Sessions created by kind.",
		},
		[]string{"kind"}, // notify|ask|approval
	)

	SessionsResolved = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "telegram_mcp_sessions_resolved_total",
			Help: "Sessions resolved by kind and status.",
		},
		[]string{"kind", "status"}, // answered|timeout|canceled
	)

	ApprovalLatency = prometheus.NewHistogramVec(
		prometheus.HistogramOpts{
			Name:    "telegram_mcp_approval_latency_seconds",
			Help:    "Time between prompt sent and user tap (answered sessions only).",
			Buckets: []float64{5, 15, 30, 60, 180, 600, 1800, 3600, 7200},
		},
		[]string{"task_tag"},
	)

	PendingGauge = prometheus.NewGauge(prometheus.GaugeOpts{
		Name: "telegram_mcp_pending_sessions",
		Help: "Currently pending sessions.",
	})

	Rehydrated = prometheus.NewCounter(prometheus.CounterOpts{
		Name: "telegram_mcp_rehydrated_total",
		Help: "Sessions rehydrated from disk since boot.",
	})
)

func init() {
	prometheus.MustRegister(
		SessionsCreated, SessionsResolved,
		ApprovalLatency, PendingGauge, Rehydrated,
	)
}

func Handler() http.Handler { return promhttp.Handler() }