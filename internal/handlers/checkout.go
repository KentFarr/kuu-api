package handlers


import (
	"encoding/json"
	"net/http"

	"github.com/stripe/stripe-go/v82"
	"github.com/stripe/stripe-go/v82/checkout/session"
)


type CheckoutRequest struct {
	Game    string `json:"game"`
	PriceID string `json:"price_id"`
}

type CheckoutResponse struct {
	CheckoutURL string `json:"checkout_url"`
}

type CheckoutHandler struct {
	StripeKey string
	SuccessURL string
	CancelURL  string
}



func (h *CheckoutHandler) HandleCheckout(w http.ResponseWriter, r *http.Request) {
	var req CheckoutRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid request body", http.StatusBadRequest)
		return
	}
	if req.PriceID == "" || req.Game == "" {
		http.Error(w, "game and price_id are required", http.StatusBadRequest)
		return
	}

	stripe.Key = h.StripeKey

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
