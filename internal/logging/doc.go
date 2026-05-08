// Package logging configures slog for the service and threads a per-request
// logger through context.
//
// Init installs a JSON slog.Handler as the default. From Go 1.21 onward the
// stdlib log package routes through slog.Default automatically, so callers
// using log.Printf still emit structured records. Handlers should prefer
// FromContext(ctx) which yields a logger pre-bound with request_id.
package logging
