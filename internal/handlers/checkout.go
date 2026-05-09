package handlers

import (
	"encoding/json"
	"fmt"
	"net/http"
	"regexp"
	"strings"

	"github.com/stripe/stripe-go/v82"
	"github.com/stripe/stripe-go/v82/checkout/session"

	"github.com/KentFarr/kuu-api/internal/plans"
)

const maxCheckoutBodyBytes = 4 << 10

// versionPattern is intentionally permissive: panel-side validation will
// catch malformed versions when CreateServer fires. We just want to reject
// obviously malicious or oversized inputs (newlines, excessive length)
// before they hit Stripe metadata or the panel.
var versionPattern = regexp.MustCompile(`^[A-Za-z0-9._-]{1,32}$`)

// subdomainPattern ensures the chosen subdomain is DNS-safe.
var subdomainPattern = regexp.MustCompile(`^[a-z0-9-]{1,63}$`)

type CheckoutRequest struct {
	Game      string `json:"game"`
	Variant   string `json:"variant,omitempty"` // optional; empty = game default
	Version   string `json:"version,omitempty"` // optional; empty = variant default
	Subdomain string `json:"subdomain,omitempty"`
	PriceID   string `json:"price_id"`
}

type CheckoutResponse struct {
	CheckoutURL string `json:"checkout_url"`
}

type CheckoutHandler struct {
	SuccessURL     string
	CancelURL      string
	StripeCouponID string
}

func (h *CheckoutHandler) HandleCheckout(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, maxCheckoutBodyBytes)

	var req CheckoutRequest
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(&req); err != nil {
		http.Error(w, "invalid request body", http.StatusBadRequest)
		return
	}
	req.Variant = strings.TrimSpace(req.Variant)
	req.Version = strings.TrimSpace(req.Version)
	req.Subdomain = strings.TrimSpace(strings.ToLower(req.Subdomain))

	if req.PriceID == "" || req.Game == "" {
		http.Error(w, "game and price_id are required", http.StatusBadRequest)
		return
	}

	gameConfig, ok := plans.Games[req.Game]
	if !ok || !gameConfig.Configured() {
		http.Error(w, "unsupported game", http.StatusBadRequest)
		return
	}

	variant, ok := plans.ResolveVariant(req.Game, req.Variant)
	if !ok || !variant.Configured() {
		http.Error(w, "unsupported variant for game", http.StatusBadRequest)
		return
	}

	if req.Version != "" {
		if variant.VersionField == "" {
			http.Error(w, "this variant does not accept a version", http.StatusBadRequest)
			return
		}
		if !versionPattern.MatchString(req.Version) {
			http.Error(w, "version contains invalid characters or is too long", http.StatusBadRequest)
			return
		}
	}

	plan := plans.FindByPriceID(req.PriceID)
	if plan == nil {
		http.Error(w, "unsupported price", http.StatusBadRequest)
		return
	}

	// Subdomain validation: Pro (8GB) and above only.
	if req.Subdomain != "" {
		if plan.RAM < 8192 {
			http.Error(w, "subdomains are only available on Pro plans and above", http.StatusBadRequest)
			return
		}
		if !subdomainPattern.MatchString(req.Subdomain) {
			http.Error(w, "subdomain must be alphanumeric and may contain hyphens", http.StatusBadRequest)
			return
		}
	}

	// Enforce minimum RAM requirements (matching frontend)
	// Rust and FiveM require at least 8GB (8192MB)
	if (req.Game == "rust" || req.Game == "fivem") && plan.RAM < 8192 {
		http.Error(w, fmt.Sprintf("%s servers require at least 8GB of RAM", gameConfig.Name), http.StatusBadRequest)
		return
	}
	// GMod requires at least 4GB (4096MB)
	if req.Game == "gmod" && plan.RAM < 4096 {
		http.Error(w, "Garry's Mod servers require at least 4GB of RAM", http.StatusBadRequest)
		return
	}

	// Resolve which variant key landed (caller may have sent ""), so the
	// webhook reads back the same value we provisioned against. Without
	// this round-trip, a default-variant request would lose its identity
	// in metadata.
	resolvedVariant := req.Variant
	if resolvedVariant == "" {
		resolvedVariant = gameConfig.DefaultVariant
	}

	params := &stripe.CheckoutSessionParams{
		Mode: stripe.String(string(stripe.CheckoutSessionModeSubscription)),
		LineItems: []*stripe.CheckoutSessionLineItemParams{
			{
				Price:    stripe.String(req.PriceID),
				Quantity: stripe.Int64(1),
			},
		},
		SuccessURL: stripe.String(h.SuccessURL),
		CancelURL:  stripe.String(h.CancelURL),
		CustomFields: []*stripe.CheckoutSessionCustomFieldParams{
			{
				Key: stripe.String("username"),
				Label: &stripe.CheckoutSessionCustomFieldLabelParams{
					Type:   stripe.String(string(stripe.CheckoutSessionCustomFieldLabelTypeCustom)),
					Custom: stripe.String("Choose a username (optional)"),
				},
				Type: stripe.String(string(stripe.CheckoutSessionCustomFieldTypeText)),
				Text: &stripe.CheckoutSessionCustomFieldTextParams{
					MinimumLength: stripe.Int64(3),
					MaximumLength: stripe.Int64(32),
				},
				Optional: stripe.Bool(true),
			},
		},
	}

	if h.StripeCouponID != "" {
		params.Discounts = []*stripe.CheckoutSessionDiscountParams{
			{
				Coupon: stripe.String(h.StripeCouponID),
			},
		}
	}

	params.Metadata = map[string]string{
		"game":      req.Game,
		"variant":   resolvedVariant,
		"version":   req.Version, // may be empty; webhook applies VersionDefault
		"subdomain": req.Subdomain,
		"price_id":  req.PriceID,
	}

	s, err := session.New(params)
	if err != nil {
		http.Error(w, "failed to create checkout session", http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(CheckoutResponse{
		CheckoutURL: s.URL,
	})
}
