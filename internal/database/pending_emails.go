package database

import (
	"fmt"
	"time"

	"github.com/jmoiron/sqlx"
)

type PendingEmail struct {
	ID            int        `db:"id"`
	ToAddr        string     `db:"to_addr"`
	Subject       string     `db:"subject"`
	Body          string     `db:"body"`
	Attempts      int        `db:"attempts"`
	MaxAttempts   int        `db:"max_attempts"`
	LastError     *string    `db:"last_error"`
	NextAttemptAt time.Time  `db:"next_attempt_at"`
	CreatedAt     time.Time  `db:"created_at"`
	SentAt        *time.Time `db:"sent_at"`
}

func (db *DB) EnqueueEmail(toAddr, subject, body string) (int, error) {
	var id int
	err := db.conn.QueryRowx(
		`INSERT INTO pending_emails (to_addr, subject, body)
		 VALUES ($1, $2, $3)
		 RETURNING id`,
		toAddr, subject, body,
	).Scan(&id)
	if err != nil {
		return 0, fmt.Errorf("failed to enqueue email: %w", err)
	}
	return id, nil
}

// ClaimDueEmails returns up to `limit` pending emails whose next_attempt_at is
// in the past, locking them with FOR UPDATE SKIP LOCKED so multiple workers
// can poll concurrently without colliding. The caller must commit or roll back
// the returned transaction.
func (db *DB) ClaimDueEmails(limit int) ([]PendingEmail, *sqlx.Tx, error) {
	tx, err := db.conn.Beginx()
	if err != nil {
		return nil, nil, fmt.Errorf("failed to begin tx: %w", err)
	}

	var emails []PendingEmail
	err = tx.Select(&emails,
		`SELECT * FROM pending_emails
		 WHERE sent_at IS NULL
		   AND attempts < max_attempts
		   AND next_attempt_at <= NOW()
		 ORDER BY next_attempt_at
		 LIMIT $1
		 FOR UPDATE SKIP LOCKED`,
		limit,
	)
	if err != nil {
		_ = tx.Rollback()
		return nil, nil, fmt.Errorf("failed to claim due emails: %w", err)
	}

	return emails, tx, nil
}

func (db *DB) MarkEmailSent(id int) error {
	_, err := db.conn.Exec(
		`UPDATE pending_emails
		 SET sent_at = NOW(), last_error = NULL
		 WHERE id = $1`,
		id,
	)
	return err
}

func (db *DB) MarkEmailSentTx(tx *sqlx.Tx, id int) error {
	_, err := tx.Exec(
		`UPDATE pending_emails
		 SET sent_at = NOW(), last_error = NULL
		 WHERE id = $1`,
		id,
	)
	return err
}

// ListPendingEmails returns unsent emails ordered by next_attempt_at, with
// a hard cap so a backed-up queue doesn't OOM the admin endpoint.
func (db *DB) ListPendingEmails(limit int) ([]PendingEmail, error) {
	var emails []PendingEmail
	err := db.conn.Select(&emails,
		`SELECT * FROM pending_emails
		 WHERE sent_at IS NULL
		 ORDER BY next_attempt_at
		 LIMIT $1`,
		limit,
	)
	if err != nil {
		return nil, fmt.Errorf("failed to list pending emails: %w", err)
	}
	return emails, nil
}

// ResetEmailForRetry forces the worker to pick the row up on its next tick
// by clearing the backoff. Useful when SMTP just came back online and ops
// doesn't want to wait out the exponential backoff. Doesn't reset the
// attempt counter — if the row had hit max_attempts it stays terminal.
func (db *DB) ResetEmailForRetry(id int) error {
	res, err := db.conn.Exec(
		`UPDATE pending_emails
		 SET next_attempt_at = NOW()
		 WHERE id = $1 AND sent_at IS NULL`,
		id,
	)
	if err != nil {
		return err
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return fmt.Errorf("pending email %d not found or already sent", id)
	}
	return nil
}

func (db *DB) MarkEmailFailed(id int, errMsg string, nextAttemptAt time.Time) error {
	_, err := db.conn.Exec(
		`UPDATE pending_emails
		 SET attempts = attempts + 1,
		     last_error = $2,
		     next_attempt_at = $3
		 WHERE id = $1`,
		id, errMsg, nextAttemptAt,
	)
	return err
}

func (db *DB) MarkEmailFailedTx(tx *sqlx.Tx, id int, errMsg string, nextAttemptAt time.Time) error {
	_, err := tx.Exec(
		`UPDATE pending_emails
		 SET attempts = attempts + 1,
		     last_error = $2,
		     next_attempt_at = $3
		 WHERE id = $1`,
		id, errMsg, nextAttemptAt,
	)
	return err
}
