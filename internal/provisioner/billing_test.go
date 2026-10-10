package provisioner

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/KentFarr/kuu-api/internal/database"
)

// billingFixture is a provisioned server plus handles to check its state as
// Stripe events arrive.
type billingFixture struct {
	t        *testing.T
	p        *Provisioner
	ptero    *fakePtero
	store    *fakeStore
	queue    *fakeQueue
	serverID int // pterodactyl server ID
}

func newBillingFixture(t *testing.T) *billingFixture {
	t.Helper()
	p, ptero, store, _, queue := newTestProvisioner(t)
	res, err := p.ProvisionServer(context.Background(), validRequest())
	if err != nil {
		t.Fatalf("ProvisionServer: %v", err)
	}
	queue.sent = nil // drop the welcome email
	return &billingFixture{t: t, p: p, ptero: ptero, store: store, queue: queue, serverID: res.ServerID}
}

func (f *billingFixture) paymentFails(attempt int64) {
	f.t.Helper()
	err := f.p.RecordPaymentFailure(context.Background(), PaymentFailure{
		StripeSubID:   "sub_123",
		CustomerEmail: "alice@example.com",
		InvoiceURL:    "https://invoice.stripe.test/i/1",
		AttemptCount:  attempt,
	})
	if err != nil {
		f.t.Fatalf("RecordPaymentFailure: %v", err)
	}
}

func (f *billingFixture) suspend(reason string) {
	f.t.Helper()
	if err := f.p.SuspendServer(context.Background(), "sub_123", reason); err != nil {
		f.t.Fatalf("SuspendServer: %v", err)
	}
}

func (f *billingFixture) paymentSucceeds() {
	f.t.Helper()
	if err := f.p.RestoreAfterPayment(context.Background(), "sub_123"); err != nil {
		f.t.Fatalf("RestoreAfterPayment: %v", err)
	}
}

func (f *billingFixture) subscription() *database.Subscription {
	f.t.Helper()
	sub, err := f.store.GetSubscriptionByStripeID("sub_123")
	if err != nil {
		f.t.Fatal(err)
	}
	return sub
}

// expect checks the subscription status, the server row status and whether
// the panel server is suspended.
func (f *billingFixture) expect(subStatus, serverStatus string, panelSuspended bool) {
	f.t.Helper()
	if got := f.subscription().Status; got != subStatus {
		f.t.Errorf("subscription status = %q, want %q", got, subStatus)
	}
	for _, s := range f.store.servers {
		if s.Status != serverStatus {
			f.t.Errorf("server row status = %q, want %q", s.Status, serverStatus)
		}
	}
	if got := f.ptero.suspended[f.serverID]; got != panelSuspended {
		f.t.Errorf("panel suspended = %v, want %v", got, panelSuspended)
	}
}

func TestPaymentFailsThenRecovers(t *testing.T) {
	f := newBillingFixture(t)

	f.paymentFails(1)
	f.expect(database.SubscriptionPastDue, "active", false)

	f.paymentSucceeds()
	f.expect(database.SubscriptionActive, "active", false)
}

func TestPaymentFailsTwiceThenStripeCancels(t *testing.T) {
	f := newBillingFixture(t)

	f.paymentFails(1)
	f.paymentFails(2)
	f.expect(database.SubscriptionPastDue, "active", false)

	// "Cancel subscription" setting: status canceled, then deleted.
	f.suspend(database.SuspendReasonCancelled)
	f.expect(database.SubscriptionSuspended, "suspended", true)

	// A cancelled subscription is never brought back, even if an old
	// invoice is paid.
	f.paymentSucceeds()
	f.expect(database.SubscriptionSuspended, "suspended", true)
}

func TestPaymentFailsThenSubscriptionDeleted(t *testing.T) {
	f := newBillingFixture(t)

	f.paymentFails(1)
	f.suspend(database.SuspendReasonCancelled)

	f.expect(database.SubscriptionSuspended, "suspended", true)
	if r := f.subscription().SuspendedReason; r == nil || *r != database.SuspendReasonCancelled {
		t.Errorf("suspended reason = %v, want cancelled", r)
	}
}

func TestUnpaidSubscriptionRestoredWhenPaid(t *testing.T) {
	f := newBillingFixture(t)

	f.paymentFails(1)
	f.suspend(database.SuspendReasonNonPayment) // "mark unpaid" setting
	f.expect(database.SubscriptionSuspended, "suspended", true)

	f.paymentSucceeds()
	f.expect(database.SubscriptionActive, "active", false)
	if sub := f.subscription(); sub.CancelledAt != nil || sub.SuspendedReason != nil {
		t.Errorf("reactivated subscription still carries suspension: %+v", sub)
	}
}

// Unpaid, then cancelled: the reason becomes cancelled (so a late payment
// can't restore it), but the reaper's clock keeps the first suspension time.
func TestSecondSuspensionKeepsReaperClock(t *testing.T) {
	f := newBillingFixture(t)

	f.suspend(database.SuspendReasonNonPayment)
	first := *f.subscription().CancelledAt
	time.Sleep(time.Millisecond)
	f.suspend(database.SuspendReasonCancelled)

	sub := f.subscription()
	if !sub.CancelledAt.Equal(first) {
		t.Errorf("cancelled_at moved from %s to %s; the grace period restarted", first, sub.CancelledAt)
	}
	if *sub.SuspendedReason != database.SuspendReasonCancelled {
		t.Errorf("reason = %q, want cancelled", *sub.SuspendedReason)
	}
}

func TestPaymentFailedEmailsOnFirstAttemptOnly(t *testing.T) {
	f := newBillingFixture(t)

	f.paymentFails(1)
	f.paymentFails(2)
	f.paymentFails(3)

	if len(f.queue.sent) != 1 {
		t.Fatalf("payment-failed emails = %d, want 1", len(f.queue.sent))
	}
	e := f.queue.sent[0]
	if e.To != "alice@example.com" || !strings.Contains(e.Body, "https://invoice.stripe.test/i/1") {
		t.Errorf("email = %+v, want to alice@example.com with the invoice link", e)
	}
}

func TestPaymentFailedEmailErrorIsNotFatal(t *testing.T) {
	f := newBillingFixture(t)
	f.queue.fail["Enqueue"] = true

	f.paymentFails(1) // fails the test if an error is returned
	f.expect(database.SubscriptionPastDue, "active", false)
}

func TestRestoreAfterPaymentEdgeCases(t *testing.T) {
	t.Run("unknown subscription is ignored", func(t *testing.T) {
		p, _, _, _, _ := newTestProvisioner(t)
		if err := p.RestoreAfterPayment(context.Background(), "sub_unknown"); err != nil {
			t.Errorf("err = %v, want nil (first invoice can arrive before provisioning)", err)
		}
	})

	t.Run("healthy subscription is untouched", func(t *testing.T) {
		f := newBillingFixture(t)
		f.paymentSucceeds()
		f.expect(database.SubscriptionActive, "active", false)
	})

	t.Run("server already reaped", func(t *testing.T) {
		f := newBillingFixture(t)
		f.suspend(database.SuspendReasonNonPayment)
		for id := range f.store.servers {
			delete(f.store.servers, id)
		}

		f.paymentSucceeds() // logged for manual follow-up, not an error
		if got := f.subscription().Status; got != database.SubscriptionSuspended {
			t.Errorf("subscription status = %q, want it left suspended", got)
		}
	})

	t.Run("panel unsuspend fails", func(t *testing.T) {
		f := newBillingFixture(t)
		f.suspend(database.SuspendReasonNonPayment)
		f.ptero.fail["UnsuspendServer"] = true

		err := f.p.RestoreAfterPayment(context.Background(), "sub_123")
		if !errors.Is(err, errInjected) {
			t.Fatalf("err = %v, want the injected failure so Stripe retries", err)
		}
		// The database must not claim the server is back.
		f.expect(database.SubscriptionSuspended, "suspended", true)
	})
}
