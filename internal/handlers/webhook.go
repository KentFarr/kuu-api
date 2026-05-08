package handlers

import (
	"encoding/json"
	"io"
	"log"
	"net/http"

	"github.com/stripe/stripe-go/v82"
	"github.com/stripe/stripe-go/v82/webhook"

	"github.com/KentFarr/kuu-api/internal/provisioner"
)

type WebhookHandler struct {
	WebhookSecret string
	Provisioner   *provisioner.Provisioner
}

func (h *WebhookHandler) HandleWebhook(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(r.Body)
	if err != nil {
		http.Error(w, "failed to read body", http.StatusBadRequest)
		return
	}

	sigHeader := r.Header.Get("Stripe-Signature")
	event, err := webhook.ConstructEvent(body, sigHeader, h.WebhookSecret)
	if err != nil {
		log.Printf("webhook signature verification failed: %v", err)
		http.Error(w, "invalid signature", http.StatusUnauthorized)
		return
	}

	log.Printf("received webhook event: %s", event.Type)

	switch event.Type {
	case "checkout.session.completed":
		h.handleCheckoutCompleted(event)
	case "customer.subscription.deleted":
		h.handleSubscriptionCancelled(event)
	case "invoice.payment_failed":
		h.handlePaymentFailed(event)
	default:
		log.Printf("unhandled event type: %s", event.Type)
	}

	w.WriteHeader(http.StatusOK)
	json.NewEncoder(w).Encode(map[string]string{"status": "received"})
}

func (h *WebhookHandler) handleCheckoutCompleted(event stripe.Event) {
	var session stripe.CheckoutSession
	if err := json.Unmarshal(event.Data.Raw, &session); err != nil {
		log.Printf("failed to parse checkout session: %v", err)
		return
	}

	customerEmail := session.CustomerDetails.Email
	game := session.Metadata["game"]
	priceID := session.Metadata["price_id"]
	subscriptionID := session.Subscription.ID

	log.Printf("new checkout completed: email=%s game=%s price=%s subscription=%s",
		customerEmail, game, priceID, subscriptionID)

	result, err := h.Provisioner.ProvisionServer(customerEmail, game, priceID)
	if err != nil {
		log.Printf("provisioning failed: %v", err)
		return
	}

	log.Printf("server provisioned successfully: user=%s server=%s port=%d",
		result.Username, result.ServerName, result.Port)
}


func (h *WebhookHandler) handleSubscriptionCancelled(event stripe.Event) {
	var sub stripe.Subscription
	if err := json.Unmarshal(event.Data.Raw, &sub); err != nil {
		log.Printf("failed to parse subscription: %v", err)
		return
	}

	log.Printf("subscription cancelled: id=%s customer=%s",
		sub.ID, sub.Customer.ID)

	// TODO: call provisioner to suspend/delete server
}

func (h *WebhookHandler) handlePaymentFailed(event stripe.Event) {
	var invoice stripe.Invoice
	if err := json.Unmarshal(event.Data.Raw, &invoice); err != nil {
		log.Printf("failed to parse invoice: %v", err)
		return
	}

	log.Printf("payment failed: customer=%s subscription=%s",
		invoice.Customer.ID, invoice.Parent.SubscriptionDetails.Subscription.ID)

	// TODO: suspend server and notify customer
}