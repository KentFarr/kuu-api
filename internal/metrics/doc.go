// Package metrics defines the Prometheus instruments emitted by kuu-api.
//
// Counters and histograms are registered via promauto so their lifetimes
// are tied to the default registry. DBStatsCollector runs as a goroutine,
// polling sql.DB.Stats() on a 10s ticker into pool gauges; it stops when
// the supplied context is done so it shuts down cleanly under
// signal.NotifyContext.
//
// Cardinality is bounded deliberately: the Pterodactyl client labels by
// route template (POST /api/application/users/:id) rather than the raw
// path, so per-customer IDs don't fan out into a label explosion.
package metrics
