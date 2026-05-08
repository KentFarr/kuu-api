package metrics

import (
	"context"
	"database/sql"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
)

// All exported counters/histograms live here so call sites import a single
// package. Names follow Prometheus conventions: snake_case, `_total` suffix
// for counters, `_seconds` for durations.

var (
	WebhookEvents = promauto.NewCounterVec(
		prometheus.CounterOpts{
			Name: "kuu_webhook_events_total",
			Help: "Webhook events processed, by Stripe event type and outcome (ok|error|skipped|unhandled).",
		},
		[]string{"type", "outcome"},
	)

	ProvisionDuration = promauto.NewHistogramVec(
		prometheus.HistogramOpts{
			Name:    "kuu_provision_duration_seconds",
			Help:    "End-to-end ProvisionServer duration, by outcome (ok|error). Buckets cover sub-second to multi-minute since Pterodactyl can be slow.",
			Buckets: []float64{0.1, 0.25, 0.5, 1, 2, 5, 10, 30, 60, 120},
		},
		[]string{"outcome"},
	)

	PterodactylRequestDuration = promauto.NewHistogramVec(
		prometheus.HistogramOpts{
			Name:    "kuu_pterodactyl_request_duration_seconds",
			Help:    "Pterodactyl panel request duration, by route label and outcome.",
			Buckets: []float64{0.05, 0.1, 0.25, 0.5, 1, 2, 5, 10, 30},
		},
		[]string{"route", "outcome"},
	)

	EmailSends = promauto.NewCounterVec(
		prometheus.CounterOpts{
			Name: "kuu_email_sends_total",
			Help: "Outbound emails attempted, by outcome (sent|failed).",
		},
		[]string{"outcome"},
	)

	RateLimiterEntries = promauto.NewGauge(
		prometheus.GaugeOpts{
			Name: "kuu_rate_limiter_entries",
			Help: "Number of IP buckets currently held by the checkout rate limiter.",
		},
	)

	dbOpenConnections = promauto.NewGauge(prometheus.GaugeOpts{
		Name: "kuu_db_open_connections",
		Help: "Open connections in the sqlx pool.",
	})
	dbInUseConnections = promauto.NewGauge(prometheus.GaugeOpts{
		Name: "kuu_db_in_use_connections",
		Help: "Connections currently in use.",
	})
	dbIdleConnections = promauto.NewGauge(prometheus.GaugeOpts{
		Name: "kuu_db_idle_connections",
		Help: "Connections currently idle in the pool.",
	})
	dbWaitCount = promauto.NewGauge(prometheus.GaugeOpts{
		Name: "kuu_db_wait_count",
		Help: "Cumulative count of times a goroutine had to wait for a connection.",
	})
)

// DBStatsCollector polls db.Stats() so the Prometheus scrape gets a
// fresh snapshot. Cheap (a single struct copy) so 10s is fine.
type DBStatsCollector struct {
	DB       *sql.DB
	Interval time.Duration
}

func (c *DBStatsCollector) Run(ctx context.Context) {
	interval := c.Interval
	if interval == 0 {
		interval = 10 * time.Second
	}
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		c.snapshot()
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

func (c *DBStatsCollector) snapshot() {
	if c.DB == nil {
		return
	}
	s := c.DB.Stats()
	dbOpenConnections.Set(float64(s.OpenConnections))
	dbInUseConnections.Set(float64(s.InUse))
	dbIdleConnections.Set(float64(s.Idle))
	dbWaitCount.Set(float64(s.WaitCount))
}

// Outcome string constants used as label values so we don't typo them at
// call sites and end up with a high-cardinality mess.
const (
	OutcomeOK        = "ok"
	OutcomeError     = "error"
	OutcomeSkipped   = "skipped"
	OutcomeUnhandled = "unhandled"
	OutcomeSent      = "sent"
	OutcomeFailed    = "failed"
)
