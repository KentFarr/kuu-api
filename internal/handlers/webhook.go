package handlers

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/stripe/stripe-go/v82"
	"github.com/stripe/stripe-go/v82/webhook"

	"github.com/KentFarr/kuu-api/internal/database"
	"github.com/KentFarr/kuu-api/internal/logging"
	"github.com/KentFarr/kuu-api/internal/metrics"
	"github.com/KentFarr/kuu-api/internal/provisioner"
)

const maxWebhookBodyBytes = 1 << 20

// webhookProvisioner is the part of *provisioner.Provisioner the webhook
// handler drives.
type webhookProvisioner interface {
	ProvisionServer(ctx context.Context, req provisioner.Request) (*provisioner.ProvisionResult, error)
	SuspendServer(ctx context.Context, stripeSubID, reason string) error
	RecordPaymentFailure(ctx context.Context, f provisioner.PaymentFailure) error
	RestoreAfterPayment(ctx context.Context, stripeSubID string) error
}

// webhookEventStore is the part of *database.DB that enforces webhook
// idempotency.
type webhookEventStore interface {
	ClaimWebhookEvent(eventID string) (bool, error)
	ReleaseWebhookEvent(eventID string) error
}

type WebhookHandler struct {
	WebhookSecret string
	Provisioner   webhookProvisioner
	DB            webhookEventStore
}

func (h *WebhookHandler) HandleWebhook(w http.ResponseWriter, r *http.Request) {
	logger := logging.FromContext(r.Context())

	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxWebhookBodyBytes))
	if err != nil {
		http.Error(w, "failed to read body", http.StatusBadRequest)
		return
	}

	sigHeader := r.Header.Get("Stripe-Signature")
	// IgnoreAPIVersionMismatch keeps us from hard-failing when Stripe rolls
	// forward to a newer API version than this SDK was built against. The
	// shapes of the events we care about (checkout.session.completed,
	// customer.subscription.deleted, invoice.payment_failed) are stable, so
	// we'd rather keep processing than drop events. Bumping stripe-go is
	// still the right move on a slower cadence — this just keeps the
	// pipeline alive in the meantime.
	event, err := webhook.ConstructEventWithOptions(body, sigHeader, h.WebhookSecret,
		webhook.ConstructEventOptions{IgnoreAPIVersionMismatch: true})
	if err != nil {
		logger.Warn("webhook signature verification failed", "err", err)
		http.Error(w, "invalid signature", http.StatusUnauthorized)
		return
	}

	logger = logger.With("event_id", event.ID, "event_type", string(event.Type))

	claimed, err := h.DB.ClaimWebhookEvent(event.ID)
	if err != nil {
		logger.Error("failed to claim webhook event", "err", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	if !claimed {
		logger.Info("event already processed, skipping")
		metrics.WebhookEvents.WithLabelValues(string(event.Type), metrics.OutcomeSkipped).Inc()
		w.WriteHeader(http.StatusOK)
		return
	}

	logger.Info("processing webhook event")

	ctx := r.Context()
	var processErr error
	outcome := metrics.OutcomeOK
	switch event.Type {
	case "checkout.session.completed":
		processErr = h.handleCheckoutCompleted(ctx, event)
	case "customer.subscription.deleted":
		processErr = h.handleSubscriptionCancelled(ctx, event)
	case "customer.subscription.updated":
		processErr = h.handleSubscriptionUpdated(ctx, event)
	case "invoice.payment_failed":
		processErr = h.handlePaymentFailed(ctx, event)
	case "invoice.paid":
		processErr = h.handleInvoicePaid(ctx, event)
	default:
		logger.Info("unhandled event type")
		outcome = metrics.OutcomeUnhandled
	}

	if processErr != nil {
		logger.Error("webhook processing failed", "err", processErr)
		if relErr := h.DB.ReleaseWebhookEvent(event.ID); relErr != nil {
			logger.Error("failed to release webhook event for retry", "err", relErr)
		}
		metrics.WebhookEvents.WithLabelValues(string(event.Type), metrics.OutcomeError).Inc()
		http.Error(w, "processing failed", http.StatusInternalServerError)
		return
	}

	metrics.WebhookEvents.WithLabelValues(string(event.Type), outcome).Inc()
	w.WriteHeader(http.StatusOK)
}

func (h *WebhookHandler) handleCheckoutCompleted(ctx context.Context, event stripe.Event) error {
	logger := logging.FromContext(ctx)

	var session stripe.CheckoutSession
	if err := json.Unmarshal(event.Data.Raw, &session); err != nil {
		return fmt.Errorf("failed to unmarshal checkout session: %w", err)
	}

	customerEmail := ""
	if session.CustomerDetails != nil {
		customerEmail = session.CustomerDetails.Email
	}
	if customerEmail == "" {
		logger.Warn("checkout.session.completed missing customer email; skipping")
		return nil
	}

	game := session.Metadata["game"]
	variant := session.Metadata["variant"]
	version := session.Metadata["version"]
	subdomain := session.Metadata["subdomain"]
	priceID := session.Metadata["price_id"]
	subscriptionID := ""
	stripeCustomerID := ""

	if session.Subscription != nil {
		subscriptionID = session.Subscription.ID
	}
	if session.Customer != nil {
		stripeCustomerID = session.Customer.ID
	}

	requestedUsername := ""
	for _, f := range session.CustomFields {
		if f.Key == "username" && f.Text != nil {
			requestedUsername = strings.TrimSpace(f.Text.Value)
			break
		}
	}

	logger.Info("checkout completed",
		"email", customerEmail,
		"game", game,
		"variant", variant,
		"version", version,
		"subdomain", subdomain,
		"price_id", priceID,
		"stripe_subscription_id", subscriptionID,
		"requested_username", requestedUsername,
	)

	result, err := h.Provisioner.ProvisionServer(ctx, provisioner.Request{
		CustomerEmail:     customerEmail,
		Game:              game,
		Variant:           variant,
		Version:           version,
		Subdomain:         subdomain,
		PriceID:           priceID,
		StripeSubID:       subscriptionID,
		StripeCustomerID:  stripeCustomerID,
		RequestedUsername: requestedUsername,
	})
	if err != nil {
		return err
	}

	logger.Info("server provisioned",
		"username", result.Username,
		"server_name", result.ServerName,
		"port", result.Port,
	)
	return nil
}

func (h *WebhookHandler) handleSubscriptionCancelled(ctx context.Context, event stripe.Event) error {
	logger := logging.FromContext(ctx)

	var sub stripe.Subscription
	if err := json.Unmarshal(event.Data.Raw, &sub); err != nil {
		return fmt.Errorf("failed to unmarshal subscription: %w", err)
	}

	logger.Info("subscription cancelled", "stripe_subscription_id", sub.ID)
	return h.Provisioner.SuspendServer(ctx, sub.ID, database.SuspendReasonCancelled)
}

// handleSubscriptionUpdated reacts to Stripe finishing its retries. What
// happens then is a Stripe dashboard setting (Billing → Revenue recovery):
// "mark unpaid" sends status unpaid here; "cancel" sends status canceled
// here and also customer.subscription.deleted. Either way we suspend. If
// the setting is "leave past due", nothing ever suspends. A return to
// active restores a server we suspended for non-payment.
func (h *WebhookHandler) handleSubscriptionUpdated(ctx context.Context, event stripe.Event) error {
	logger := logging.FromContext(ctx)

	var sub stripe.Subscription
	if err := json.Unmarshal(event.Data.Raw, &sub); err != nil {
		return fmt.Errorf("failed to unmarshal subscription: %w", err)
	}
	logger = logger.With("stripe_subscription_id", sub.ID, "status", string(sub.Status))

	switch sub.Status {
	case stripe.SubscriptionStatusUnpaid:
		logger.Warn("payment retries exhausted; suspending")
		return h.Provisioner.SuspendServer(ctx, sub.ID, database.SuspendReasonNonPayment)
	case stripe.SubscriptionStatusCanceled:
		logger.Info("subscription canceled; suspending")
		return h.Provisioner.SuspendServer(ctx, sub.ID, database.SuspendReasonCancelled)
	case stripe.SubscriptionStatusActive:
		return h.Provisioner.RestoreAfterPayment(ctx, sub.ID)
	default:
		// past_due is handled by invoice.payment_failed; other statuses
		// (trialing, incomplete, paused) don't affect the server.
		return nil
	}
}

// invoiceSubscriptionID returns the subscription an invoice belongs to, or
// "" for a one-off invoice.
func invoiceSubscriptionID(invoice *stripe.Invoice) string {
	if invoice.Parent != nil && invoice.Parent.SubscriptionDetails != nil && invoice.Parent.SubscriptionDetails.Subscription != nil {
		return invoice.Parent.SubscriptionDetails.Subscription.ID
	}
	return ""
}

// handlePaymentFailed records the failure and emails the customer, but
// doesn't suspend: Stripe retries the charge for days. See
// provisioner.RecordPaymentFailure.
func (h *WebhookHandler) handlePaymentFailed(ctx context.Context, event stripe.Event) error {
	logger := logging.FromContext(ctx)

	var invoice stripe.Invoice
	if err := json.Unmarshal(event.Data.Raw, &invoice); err != nil {
		return fmt.Errorf("failed to unmarshal invoice: %w", err)
	}

	subID := invoiceSubscriptionID(&invoice)

	customerID := ""
	if invoice.Customer != nil {
		customerID = invoice.Customer.ID
	}

	logger.Warn("payment failed",
		"stripe_customer_id", customerID,
		"stripe_subscription_id", subID,
		"attempt", invoice.AttemptCount,
	)

	if subID == "" {
		return nil
	}
	return h.Provisioner.RecordPaymentFailure(ctx, provisioner.PaymentFailure{
		StripeSubID:   subID,
		CustomerEmail: invoice.CustomerEmail,
		InvoiceURL:    invoice.HostedInvoiceURL,
		AttemptCount:  invoice.AttemptCount,
	})
}

// handleInvoicePaid restores service if this payment settles a failed one.
// It fires for every paid invoice; for a healthy subscription it's a no-op.
func (h *WebhookHandler) handleInvoicePaid(ctx context.Context, event stripe.Event) error {
	var invoice stripe.Invoice
	if err := json.Unmarshal(event.Data.Raw, &invoice); err != nil {
		return fmt.Errorf("failed to unmarshal invoice: %w", err)
	}
	subID := invoiceSubscriptionID(&invoice)
	if subID == "" {
		return nil
	}
	return h.Provisioner.RestoreAfterPayment(ctx, subID)
}
