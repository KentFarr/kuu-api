// Package pterodactyl is the application-API HTTP client.
//
// Client carries an auth header, a 30s http.Client.Timeout, and emits
// per-route metrics (the route template, not the raw path, to keep label
// cardinality bounded). Calls accept a context.Context and respect
// cancellation. APIError preserves the response status and body so
// callers can branch on status (e.g. 422 = validation, 401/403 = auth)
// and surface useful diagnostics.
//
// generateUsername is deterministic over email so retries collapse onto
// the same panel user, avoiding duplicate accounts on a Stripe redelivery.
// generatePassword uses crypto/rand and is the only copy of the secret
// the customer ever receives — it is not persisted on our side.
package pterodactyl
