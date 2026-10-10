package database

import (
	"testing"
	"time"
)

func TestPendingEmailLifecycle(t *testing.T) {
	db := newTestDB(t)

	sentID, err := db.EnqueueEmail("a@example.com", "Welcome", "body a")
	if err != nil {
		t.Fatalf("EnqueueEmail: %v", err)
	}
	failID, err := db.EnqueueEmail("b@example.com", "Welcome", "body b")
	if err != nil {
		t.Fatalf("EnqueueEmail: %v", err)
	}

	// Both are due immediately.
	emails, tx, err := db.ClaimDueEmails(10)
	if err != nil {
		t.Fatalf("ClaimDueEmails: %v", err)
	}
	if len(emails) != 2 {
		t.Fatalf("claimed %d emails, want 2", len(emails))
	}

	// One send succeeds; the other fails and is pushed into the future.
	if err := db.MarkEmailSentTx(tx, sentID); err != nil {
		t.Fatalf("MarkEmailSentTx: %v", err)
	}
	if err := db.MarkEmailFailedTx(tx, failID, "smtp down", time.Now().Add(time.Hour)); err != nil {
		t.Fatalf("MarkEmailFailedTx: %v", err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("commit: %v", err)
	}

	// Nothing is due now: one is sent, the other is backing off.
	emails, tx, err = db.ClaimDueEmails(10)
	if err != nil {
		t.Fatalf("ClaimDueEmails: %v", err)
	}
	_ = tx.Commit()
	if len(emails) != 0 {
		t.Errorf("claimed %d emails during backoff, want 0", len(emails))
	}

	pending, err := db.ListPendingEmails(10)
	if err != nil {
		t.Fatalf("ListPendingEmails: %v", err)
	}
	if len(pending) != 1 || pending[0].ID != failID || pending[0].Attempts != 1 ||
		pending[0].LastError == nil || *pending[0].LastError != "smtp down" {
		t.Fatalf("pending = %+v, want the failed email with 1 attempt and its error", pending)
	}

	// An operator retry makes it due again.
	if err := db.ResetEmailForRetry(failID); err != nil {
		t.Fatalf("ResetEmailForRetry: %v", err)
	}
	emails, tx, err = db.ClaimDueEmails(10)
	if err != nil {
		t.Fatalf("ClaimDueEmails: %v", err)
	}
	_ = tx.Commit()
	if len(emails) != 1 || emails[0].ID != failID {
		t.Errorf("after retry claimed %+v, want the failed email", emails)
	}

	// Retrying a sent email is an error.
	if err := db.ResetEmailForRetry(sentID); err == nil {
		t.Error("ResetEmailForRetry on a sent email: expected an error")
	}
}

func TestClaimDueEmailsSkipsExhaustedRows(t *testing.T) {
	db := newTestDB(t)

	id, err := db.EnqueueEmail("a@example.com", "s", "b")
	if err != nil {
		t.Fatal(err)
	}
	mustExec(t, db, `UPDATE pending_emails SET attempts = max_attempts WHERE id = $1`, id)

	emails, tx, err := db.ClaimDueEmails(10)
	if err != nil {
		t.Fatal(err)
	}
	_ = tx.Commit()
	if len(emails) != 0 {
		t.Errorf("claimed an email that hit max_attempts: %+v", emails)
	}
}

// Two workers polling at once must not both get the same row.
func TestClaimDueEmailsSkipsLockedRows(t *testing.T) {
	db := newTestDB(t)
	if _, err := db.EnqueueEmail("a@example.com", "s", "b"); err != nil {
		t.Fatal(err)
	}

	first, tx1, err := db.ClaimDueEmails(10)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx1.Rollback() }()

	second, tx2, err := db.ClaimDueEmails(10)
	if err != nil {
		t.Fatal(err)
	}
	_ = tx2.Commit()

	if len(first) != 1 || len(second) != 0 {
		t.Errorf("first claim got %d, second got %d; want 1 and 0", len(first), len(second))
	}
}

func TestPruneWebhookEvents(t *testing.T) {
	db := newTestDB(t)
	for _, id := range []string{"evt_old", "evt_new"} {
		if _, err := db.ClaimWebhookEvent(id); err != nil {
			t.Fatal(err)
		}
	}
	mustExec(t, db, `UPDATE webhook_events SET processed_at = NOW() - INTERVAL '31 days' WHERE event_id = 'evt_old'`)

	n, err := db.PruneWebhookEvents(30 * 24 * time.Hour)
	if err != nil {
		t.Fatalf("PruneWebhookEvents: %v", err)
	}
	if n != 1 {
		t.Errorf("pruned %d rows, want 1", n)
	}
	// The pruned event can be claimed again; the recent one can't.
	if ok, _ := db.ClaimWebhookEvent("evt_old"); !ok {
		t.Error("old event still present after prune")
	}
	if ok, _ := db.ClaimWebhookEvent("evt_new"); ok {
		t.Error("recent event was pruned")
	}
}
