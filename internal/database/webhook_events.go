package database

import "time"

func (db *DB) ClaimWebhookEvent(eventID string) (bool, error) {
	res, err := db.conn.Exec(
		"INSERT INTO webhook_events (event_id) VALUES ($1) ON CONFLICT (event_id) DO NOTHING",
		eventID,
	)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, err
	}
	return n == 1, nil
}

func (db *DB) ReleaseWebhookEvent(eventID string) error {
	_, err := db.conn.Exec("DELETE FROM webhook_events WHERE event_id = $1", eventID)
	return err
}

// PruneWebhookEvents removes processed events older than the given cutoff.
// The webhook_events table only exists to enforce idempotency against Stripe
// retries, and Stripe gives up retrying within a few days — so a row from
// last quarter is dead weight, indexed but useless. Returns the number of
// rows deleted so the caller can log it.
func (db *DB) PruneWebhookEvents(olderThan time.Duration) (int64, error) {
	cutoff := time.Now().Add(-olderThan)
	res, err := db.conn.Exec(
		"DELETE FROM webhook_events WHERE processed_at < $1",
		cutoff,
	)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}
