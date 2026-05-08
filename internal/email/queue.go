package email

import (
	"context"
	"log"
	"math"
	"time"

	"github.com/KentFarr/kuu-api/internal/database"
	"github.com/KentFarr/kuu-api/internal/metrics"
)

const (
	defaultBatchSize    = 10
	defaultPollInterval = 30 * time.Second
	maxBackoff          = 1 * time.Hour
)

// Queue persists outbound emails and retries them in the background, so that
// SMTP outages or panics during inline send don't lose welcome credentials —
// which are the customer's only copy of their generated password.
type Queue struct {
	DB           *database.DB
	Mailer       *Mailer
	BatchSize    int
	PollInterval time.Duration
}

func NewQueue(db *database.DB, mailer *Mailer) *Queue {
	return &Queue{
		DB:           db,
		Mailer:       mailer,
		BatchSize:    defaultBatchSize,
		PollInterval: defaultPollInterval,
	}
}

// Enqueue persists an email for later delivery. The caller is responsible for
// providing a fully-rendered subject + body — the worker does no further
// templating, which keeps retries idempotent.
func (q *Queue) Enqueue(toAddr, subject, body string) error {
	id, err := q.DB.EnqueueEmail(toAddr, subject, body)
	if err != nil {
		return err
	}
	log.Printf("email enqueued id=%d to=%s", id, toAddr)
	return nil
}

// Run blocks until ctx is cancelled, polling the pending_emails table on
// PollInterval and dispatching due rows. Designed to be launched in its own
// goroutine from main; the parent should cancel ctx during shutdown.
func (q *Queue) Run(ctx context.Context) {
	ticker := time.NewTicker(q.PollInterval)
	defer ticker.Stop()

	q.processOnce(ctx)

	for {
		select {
		case <-ctx.Done():
			log.Println("email queue worker stopping")
			return
		case <-ticker.C:
			q.processOnce(ctx)
		}
	}
}

func (q *Queue) processOnce(ctx context.Context) {
	if q.Mailer == nil {
		return
	}
	if err := ctx.Err(); err != nil {
		return
	}

	emails, commit, rollback, err := q.DB.ClaimDueEmails(q.BatchSize)
	if err != nil {
		log.Printf("email queue: failed to claim due emails: %v", err)
		return
	}
	if len(emails) == 0 {
		_ = commit()
		return
	}

	for _, e := range emails {
		if ctx.Err() != nil {
			_ = rollback()
			return
		}
		q.attemptSend(e)
	}

	if err := commit(); err != nil {
		log.Printf("email queue: failed to commit batch: %v", err)
	}
}

func (q *Queue) attemptSend(e database.PendingEmail) {
	err := q.Mailer.Send(e.ToAddr, e.Subject, e.Body)
	if err == nil {
		metrics.EmailSends.WithLabelValues(metrics.OutcomeSent).Inc()
		if markErr := q.DB.MarkEmailSent(e.ID); markErr != nil {
			log.Printf("email queue: sent email id=%d but failed to mark sent: %v", e.ID, markErr)
		}
		return
	}

	metrics.EmailSends.WithLabelValues(metrics.OutcomeFailed).Inc()
	nextAttempt := time.Now().Add(backoffFor(e.Attempts + 1))
	if markErr := q.DB.MarkEmailFailed(e.ID, err.Error(), nextAttempt); markErr != nil {
		log.Printf("email queue: failed to record failure for email id=%d: %v", e.ID, markErr)
	} else {
		log.Printf("email queue: send failed id=%d attempts=%d next=%s err=%v",
			e.ID, e.Attempts+1, nextAttempt.Format(time.RFC3339), err)
	}
}

// backoffFor returns the delay before the Nth retry attempt. 1 → 1m,
// 2 → 4m, 3 → 9m, 4 → 16m, 5 → 25m, capped at maxBackoff so a stuck row
// doesn't drift weeks into the future.
func backoffFor(attempt int) time.Duration {
	d := time.Duration(math.Pow(float64(attempt), 2)) * time.Minute
	if d > maxBackoff {
		return maxBackoff
	}
	return d
}
