package provisioner

import (
	"context"
	"fmt"

	"github.com/KentFarr/kuu-api/internal/database"
	"github.com/KentFarr/kuu-api/internal/email"
	"github.com/KentFarr/kuu-api/internal/logging"
)

// PaymentFailure describes one failed renewal attempt from an
// invoice.payment_failed event.
type PaymentFailure struct {
	StripeSubID   string
	CustomerEmail string
	InvoiceURL    string // Stripe's hosted invoice page
	AttemptCount  int64  // 1 on the first failure, then once per Stripe retry
}

// RecordPaymentFailure marks the subscription past_due without suspending
// anything: Stripe keeps retrying the charge for days, and a temporarily
// declined card shouldn't cost the customer their server. Suspension
// happens only when Stripe gives up (see SuspendServer).
//
// The customer is emailed once, on the first failed attempt, with a link to
// pay the invoice with another card.
func (p *Provisioner) RecordPaymentFailure(ctx context.Context, f PaymentFailure) error {
	logger := logging.FromContext(ctx).With("stripe_subscription_id", f.StripeSubID, "attempt", f.AttemptCount)

	if err := p.DB.MarkSubscriptionPastDue(f.StripeSubID); err != nil {
		return fmt.Errorf("failed to mark subscription past due: %w", err)
	}
	logger.Info("payment failed; subscription past due, server left running")

	if p.EmailQueue == nil || f.AttemptCount > 1 || f.CustomerEmail == "" || f.InvoiceURL == "" {
		return nil
	}
	subject, body := email.RenderPaymentFailed(f.InvoiceURL)
	if err := p.EmailQueue.Enqueue(f.CustomerEmail, subject, body); err != nil {
		// The status change is what matters; a missing reminder email
		// shouldn't make Stripe redeliver the event.
		logger.Warn("failed to enqueue payment-failed email", "err", err)
	}
	return nil
}

// RestoreAfterPayment handles a successful payment or a subscription that
// is active again. A past_due subscription simply returns to active. A
// subscription we suspended for non-payment is unsuspended on the panel and
// reactivated. Anything else (active already, or cancelled by the customer)
// is left alone.
//
// It runs for every paid invoice, including the first one, which can
// arrive before checkout has provisioned anything, so an unknown
// subscription is not an error.
func (p *Provisioner) RestoreAfterPayment(ctx context.Context, stripeSubID string) error {
	logger := logging.FromContext(ctx).With("stripe_subscription_id", stripeSubID)

	sub, err := p.DB.GetSubscriptionByStripeID(stripeSubID)
	if err != nil {
		logger.Info("payment for a subscription we don't track yet; ignoring", "err", err)
		return nil
	}

	switch {
	case sub.Status == database.SubscriptionPastDue:
		if err := p.DB.ReactivateSubscription(stripeSubID); err != nil {
			return fmt.Errorf("failed to reactivate subscription: %w", err)
		}
		logger.Info("payment recovered before suspension; subscription active")
		return nil

	case sub.Status == database.SubscriptionSuspended &&
		sub.SuspendedReason != nil && *sub.SuspendedReason == database.SuspendReasonNonPayment:
		// handled below

	default:
		return nil
	}

	server, err := p.DB.GetServerBySubscription(sub.ID)
	if err != nil {
		// The reaper already deleted it. Leave the records suspended so
		// it's visible, and flag it: the customer has paid for a server
		// that no longer exists.
		logger.Error("payment recovered after the server was reaped; needs manual follow-up", "err", err)
		return nil
	}

	// Panel first, so the database never says active while the server is
	// still suspended.
	if server.PterodactylServerID > 0 {
		if err := p.PteroClient.UnsuspendServer(ctx, server.PterodactylServerID); err != nil {
			return fmt.Errorf("failed to unsuspend pterodactyl server %d: %w", server.PterodactylServerID, err)
		}
	}
	if err := p.DB.UnsuspendServer(sub.ID); err != nil {
		return fmt.Errorf("failed to unsuspend server record: %w", err)
	}
	if err := p.DB.ReactivateSubscription(stripeSubID); err != nil {
		return fmt.Errorf("failed to reactivate subscription: %w", err)
	}
	logger.Info("payment recovered; server unsuspended")
	return nil
}
