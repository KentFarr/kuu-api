package handlers

import (
	"encoding/json"
	"net/http"

	"github.com/stripe/stripe-go/v82"
	"github.com/stripe/stripe-go/v82/checkout/session"

	"github.com/KentFarr/kuu-api/internal/plans"
)

const maxCheckoutBodyBytes = 4 << 10

type CheckoutRequest struct {
	Game    string `json:"game"`
	PriceID string `json:"price_id"`
}

type CheckoutResponse struct {
	CheckoutURL string `json:"checkout_url"`
}

type CheckoutHandler struct {
	SuccessURL string
	CancelURL  string
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
	if req.PriceID == "" || req.Game == "" {
		http.Error(w, "game and price_id are required", http.StatusBadRequest)
		return
	}

	gameConfig, ok := plans.Games[req.Game]
	if !ok || !gameConfig.Configured() {
		http.Error(w, "unsupported game", http.StatusBadRequest)
		return
	}

	plan := plans.FindByPriceID(req.PriceID)
	if plan == nil {
		http.Error(w, "unsupported price", http.StatusBadRequest)
		return
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
	}
	params.Metadata = map[string]string{
		"game":     req.Game,
		"price_id": req.PriceID,
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
