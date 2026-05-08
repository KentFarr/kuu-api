package database

import (
	"fmt"
	"time"
)

type Subscription struct {
	ID                   int        `db:"id"`
	CustomerID           int        `db:"customer_id"`
	StripeSubscriptionID string     `db:"stripe_subscription_id"`
	StripePriceID        string     `db:"stripe_price_id"`
	Game                 string     `db:"game"`
	Status               string     `db:"status"`
	CreatedAt            time.Time  `db:"created_at"`
	CancelledAt          *time.Time `db:"cancelled_at"`
}

func (db *DB) CreateSubscription(customerID int, stripeSubID, stripePriceID, game string) (*Subscription, error) {
	var sub Subscription
	err := db.conn.QueryRowx(
		`INSERT INTO subscriptions (customer_id, stripe_subscription_id, stripe_price_id, game, status)
		 VALUES ($1, $2, $3, $4, 'active')
		 RETURNING *`,
		customerID, stripeSubID, stripePriceID, game,
	).StructScan(&sub)

	if err != nil {
		return nil, fmt.Errorf("failed to create subscription: %w", err)
	}

	return &sub, nil
}

func (db *DB) GetSubscriptionByStripeID(stripeSubID string) (*Subscription, error) {
	var sub Subscription
	err := db.conn.Get(&sub, "SELECT * FROM subscriptions WHERE stripe_subscription_id = $1", stripeSubID)
	if err != nil {
		return nil, fmt.Errorf("subscription not found: %w", err)
	}
	return &sub, nil
}

func (db *DB) SuspendSubscription(stripeSubID string) error {
	_, err := db.conn.Exec(
		"UPDATE subscriptions SET status = 'suspended', cancelled_at = NOW() WHERE stripe_subscription_id = $1",
		stripeSubID,
	)
	return err
}

func (db *DB) DeleteSubscription(subscriptionID int) error {
	_, err := db.conn.Exec("DELETE FROM subscriptions WHERE id = $1", subscriptionID)
	return err
}
