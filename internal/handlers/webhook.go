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
	// Subscriptions supplies live subscription status from Stripe. When
	// nil, handlers trust the status carried in the event.
	Subscriptions subscriptionStatusSource
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

// currentStatus returns the subscription's live status from Stripe, so a
// delayed or redelivered event can't act on stale state (Stripe doesn't
// guarantee event order: an old "unpaid" could otherwise arrive after
// "invoice.paid" and re-suspend a paid-up server). Without a status source
// (tests), it trusts the event's own value.
func (h *WebhookHandler) currentStatus(ctx context.Context, stripeSubID string, fromEvent stripe.SubscriptionStatus) (stripe.SubscriptionStatus, error) {
	if h.Subscriptions == nil {
		return fromEvent, nil
	}
	return h.Subscriptions.Status(ctx, stripeSubID)
}

// applySubscriptionStatus brings our records in line with Stripe's status.
// Stripe's retries end according to a dashboard setting (Billing → Revenue
// recovery): "mark unpaid" leaves the subscription unpaid, "cancel" cancels
// it. Either way we suspend. If the setting is "leave past due", nothing
// ever suspends. Active restores a server we suspended for non-payment.
func (h *WebhookHandler) applySubscriptionStatus(ctx context.Context, stripeSubID string, status stripe.SubscriptionStatus) error {
	logger := logging.FromContext(ctx).With("stripe_subscription_id", stripeSubID, "status", string(status))

	switch status {
	case stripe.SubscriptionStatusUnpaid:
		logger.Warn("payment retries exhausted; suspending")
		return h.Provisioner.SuspendServer(ctx, stripeSubID, database.SuspendReasonNonPayment)
	case stripe.SubscriptionStatusCanceled, stripe.SubscriptionStatusIncompleteExpired:
		logger.Info("subscription canceled; suspending")
		return h.Provisioner.SuspendServer(ctx, stripeSubID, database.SuspendReasonCancelled)
	case stripe.SubscriptionStatusActive:
		return h.Provisioner.RestoreAfterPayment(ctx, stripeSubID)
	default:
		// past_due is handled by invoice.payment_failed; other statuses
		// (trialing, incomplete, paused) don't affect the server.
		return nil
	}
}

func (h *WebhookHandler) handleSubscriptionUpdated(ctx context.Context, event stripe.Event) error {
	var sub stripe.Subscription
	if err := json.Unmarshal(event.Data.Raw, &sub); err != nil {
		return fmt.Errorf("failed to unmarshal subscription: %w", err)
	}
	status, err := h.currentStatus(ctx, sub.ID, sub.Status)
	if err != nil {
		return err
	}
	return h.applySubscriptionStatus(ctx, sub.ID, status)
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
// doesn't suspend while Stripe is still retrying. See
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

	status, err := h.currentStatus(ctx, subID, stripe.SubscriptionStatusPastDue)
	if err != nil {
		return err
	}
	if status != stripe.SubscriptionStatusPastDue {
		// Already recovered (a retry succeeded) or past the retries; act on
		// where the subscription is now, without a "payment failed" email.
		return h.applySubscriptionStatus(ctx, subID, status)
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
	status, err := h.currentStatus(ctx, subID, stripe.SubscriptionStatusActive)
	if err != nil {
		return err
	}
	return h.applySubscriptionStatus(ctx, subID, status)
}
