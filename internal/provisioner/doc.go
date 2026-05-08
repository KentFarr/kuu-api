// Package provisioner orchestrates the end-to-end "Stripe paid → server
// running" flow.
//
// ProvisionServer is the single entry point. It runs the steps in order:
// look up or create a Pterodactyl user, create the server on the
// configured node, persist customer + subscription + server rows in
// Postgres, and enqueue the welcome email. Each step that can be undone
// registers a deferred rollback; if a later step fails, the rollbacks
// unwind in reverse order. cleanupCtxFor preserves the request_id on
// the rollback ctx so log lines stitch up with the original request.
//
// ProvisionDuration is observed via a named-return defer so the histogram
// records latency for both successful and failed paths, labelled by
// outcome.
package provisioner
