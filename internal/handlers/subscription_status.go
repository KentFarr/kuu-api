package handlers

import (
	"context"
	"fmt"

	"github.com/stripe/stripe-go/v82"
	"github.com/stripe/stripe-go/v82/subscription"
)

// subscriptionStatusSource returns a subscription's current status.
type subscriptionStatusSource interface {
	Status(ctx context.Context, stripeSubID string) (stripe.SubscriptionStatus, error)
}

// StripeSubscriptions reads the live status from the Stripe API.
type StripeSubscriptions struct{}

func (StripeSubscriptions) Status(ctx context.Context, stripeSubID string) (stripe.SubscriptionStatus, error) {
	params := &stripe.SubscriptionParams{}
	params.Context = ctx
	sub, err := subscription.Get(stripeSubID, params)
	if err != nil {
		return "", fmt.Errorf("fetch subscription %s: %w", stripeSubID, err)
	}
	return sub.Status, nil
}
