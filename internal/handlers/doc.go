// Package handlers contains the HTTP surface of kuu-api.
//
// Public endpoints: /api/checkout (rate-limited Stripe Checkout creator)
// and /api/webhook (Stripe event sink). Operational endpoints: /livez,
// /readyz, /health, /metrics. The /admin/* surface (email retry,
// reconciliation) is registered only when ADMIN_BEARER_TOKEN is set and
// requires constant-time bearer auth on every request.
//
// Middleware in middleware.go covers: SecurityHeaders, CORS, request-ID
// minting/binding, and per-IP rate limiting. clientIP only honours
// X-Forwarded-For when the request's RemoteAddr falls inside a CIDR
// supplied by config.TrustedProxyCIDRs.
package handlers
