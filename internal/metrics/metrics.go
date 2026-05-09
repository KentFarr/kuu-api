package metrics

import (
	"context"
	"database/sql"
	"strconv"
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

	NodeMemoryUsedMB = promauto.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "kuu_node_memory_used_mb",
			Help: "Pterodactyl node allocated memory in MB.",
		},
		[]string{"node_id"},
	)
	NodeMemoryTotalMB = promauto.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "kuu_node_memory_total_mb",
			Help: "Pterodactyl node total memory in MB.",
		},
		[]string{"node_id"},
	)
	NodeFreeAllocations = promauto.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "kuu_node_free_allocations",
			Help: "Unassigned allocations available on a node (sampled from page 1, capped at 100).",
		},
		[]string{"node_id"},
	)
	ProvisionNodeChosen = promauto.NewCounterVec(
		prometheus.CounterOpts{
			Name: "kuu_provision_node_chosen_total",
			Help: "Provisioning picks per node, used to verify load balancing.",
		},
		[]string{"node_id"},
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

// NodeUtilSample is the minimal shape NodeMetricsCollector needs from a
// utilization snapshot. Defining it here keeps internal/metrics free of a
// dependency on internal/pterodactyl (and the import cycle that would
// imply).
type NodeUtilSample struct {
	NodeID          int
	MemoryUsedMB    int
	MemoryTotalMB   int
	FreeAllocations int
}

// NodeMetricsCollector ticks every Interval and refreshes the per-node
// gauges. Fetcher is wired up in main.go to a function that calls
// pterodactyl.Client.GetNodeUtilization.
type NodeMetricsCollector struct {
	NodeIDs  []int
	Fetcher  func(ctx context.Context, nodeID int) (*NodeUtilSample, error)
	Interval time.Duration
}

func (c *NodeMetricsCollector) Run(ctx context.Context) {
	interval := c.Interval
	if interval == 0 {
		interval = 30 * time.Second
	}
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		c.snapshot(ctx)
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

func (c *NodeMetricsCollector) snapshot(ctx context.Context) {
	if c.Fetcher == nil {
		return
	}
	for _, id := range c.NodeIDs {
		s, err := c.Fetcher(ctx, id)
		if err != nil || s == nil {
			continue
		}
		label := strconv.Itoa(id)
		NodeMemoryUsedMB.WithLabelValues(label).Set(float64(s.MemoryUsedMB))
		NodeMemoryTotalMB.WithLabelValues(label).Set(float64(s.MemoryTotalMB))
		NodeFreeAllocations.WithLabelValues(label).Set(float64(s.FreeAllocations))
	}
}

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
