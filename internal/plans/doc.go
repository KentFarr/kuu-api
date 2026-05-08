// Package plans is the static mapping between Stripe price IDs and the
// game-server plans we sell.
//
// Each plan binds a price ID, a human label, a Pterodactyl egg/nest, and
// resource limits (memory, disk, cpu). FindByPriceID is the lookup the
// webhook handler uses to translate a Stripe checkout.session into the
// shape Pterodactyl expects. ValidateConfigured is called at boot so a
// missing STRIPE_PRICE_* env var fails fast.
package plans
