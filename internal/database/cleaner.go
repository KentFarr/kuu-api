package database

import (
	"context"
	"log"
	"time"
)

const (
	defaultCleanInterval    = 24 * time.Hour
	defaultWebhookEventTTL  = 30 * 24 * time.Hour
	cleanInitialDelayJitter = 5 * time.Minute
)

// Cleaner periodically prunes idempotency rows that no longer serve a
// purpose. It runs in-process rather than as a separate cron because there
// is exactly one thing to do (delete old webhook_events) and adding an
// external cron just adds another moving part to operate.
type Cleaner struct {
	DB              *DB
	Interval        time.Duration
	WebhookEventTTL time.Duration
}

func NewCleaner(db *DB) *Cleaner {
	return &Cleaner{
		DB:              db,
		Interval:        defaultCleanInterval,
		WebhookEventTTL: defaultWebhookEventTTL,
	}
}

// Run blocks until ctx is cancelled. Designed to be launched in its own
// goroutine from main; the parent should cancel ctx during shutdown.
func (c *Cleaner) Run(ctx context.Context) {
	// Delay the first run so a crash-loop doesn't hammer the DB at startup.
	select {
	case <-ctx.Done():
		return
	case <-time.After(cleanInitialDelayJitter):
	}

	c.runOnce(ctx)

	ticker := time.NewTicker(c.Interval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			log.Println("db cleaner stopping")
			return
		case <-ticker.C:
			c.runOnce(ctx)
		}
	}
}

func (c *Cleaner) runOnce(ctx context.Context) {
	if ctx.Err() != nil {
		return
	}
	n, err := c.DB.PruneWebhookEvents(c.WebhookEventTTL)
	if err != nil {
		log.Printf("db cleaner: prune webhook_events failed: %v", err)
		return
	}
	if n > 0 {
		log.Printf("db cleaner: pruned %d webhook_events older than %s", n, c.WebhookEventTTL)
	}
}
