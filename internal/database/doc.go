// Package database wraps a *sql.DB with the queries kuu-api needs.
//
// Models live alongside their queries (customers.go, subscriptions.go,
// servers.go, webhook_events.go, pending_emails.go). The connection pool
// is configured in New; cleaner.go runs the daily prune of stale rows.
//
// Migrations are not run from here — see cmd/migrate, which uses
// pressly/goose against the embedded SQL in the migrations/ tree.
package database
