package handlers

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/stripe/stripe-go/v82/webhook"

	"github.com/KentFarr/kuu-api/internal/provisioner"
)

const testWebhookSecret = "whsec_test"

// fakeProvisioner records every call so tests can assert what the webhook
// asked for. Set provisionErr / suspendErr to make the next calls fail.
type fakeProvisioner struct {
	mu            sync.Mutex
	provisioned   []provisioner.Request
	suspended     []string // "subID:reason"
	paymentFailed []provisioner.PaymentFailure
	restored      []string
	calls         []string // every call in order, e.g. "suspend sub_1 cancelled"
	provisionErr  error
	suspendErr    error
}

func (f *fakeProvisioner) ProvisionServer(_ context.Context, req provisioner.Request) (*provisioner.ProvisionResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.provisionErr != nil {
		return nil, f.provisionErr
	}
	f.provisioned = append(f.provisioned, req)
	f.calls = append(f.calls, "provision "+req.CustomerEmail)
	return &provisioner.ProvisionResult{Username: "kuu_test", ServerName: "test-server"}, nil
}

func (f *fakeProvisioner) SuspendServer(_ context.Context, stripeSubID, reason string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.suspendErr != nil {
		return f.suspendErr
	}
	f.suspended = append(f.suspended, stripeSubID+":"+reason)
	f.calls = append(f.calls, "suspend "+stripeSubID+" "+reason)
	return nil
}

func (f *fakeProvisioner) RecordPaymentFailure(_ context.Context, pf provisioner.PaymentFailure) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.paymentFailed = append(f.paymentFailed, pf)
	f.calls = append(f.calls, "payment_failed "+pf.StripeSubID)
	return nil
}

func (f *fakeProvisioner) RestoreAfterPayment(_ context.Context, stripeSubID string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.restored = append(f.restored, stripeSubID)
	f.calls = append(f.calls, "restore "+stripeSubID)
	return nil
}

// fakeEventStore behaves like the webhook_events table: a claim succeeds
// once per event ID until the event is released.
type fakeEventStore struct {
	mu       sync.Mutex
	claimed  map[string]bool
	released []string
	claimErr error
}

func newFakeEventStore() *fakeEventStore {
	return &fakeEventStore{claimed: map[string]bool{}}
}

func (f *fakeEventStore) ClaimWebhookEvent(eventID string) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.claimErr != nil {
		return false, f.claimErr
	}
	if f.claimed[eventID] {
		return false, nil
	}
	f.claimed[eventID] = true
	return true, nil
}

func (f *fakeEventStore) ReleaseWebhookEvent(eventID string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.claimed, eventID)
	f.released = append(f.released, eventID)
	return nil
}

func newTestWebhookHandler() (*WebhookHandler, *fakeProvisioner, *fakeEventStore) {
	prov := &fakeProvisioner{}
	store := newFakeEventStore()
	return &WebhookHandler{WebhookSecret: testWebhookSecret, Provisioner: prov, DB: store}, prov, store
}

// stripeEvent builds the JSON body of a Stripe event wrapping object.
func stripeEvent(t *testing.T, id, eventType string, object any) []byte {
	t.Helper()
	raw, err := json.Marshal(object)
	if err != nil {
		t.Fatalf("marshal event object: %v", err)
	}
	body, err := json.Marshal(map[string]any{
		"id":     id,
		"object": "event",
		"type":   eventType,
		"data":   map[string]json.RawMessage{"object": raw},
	})
	if err != nil {
		t.Fatalf("marshal event: %v", err)
	}
	return body
}

// checkoutCompletedEvent is a minimal checkout.session.completed event with
// everything the handler reads.
func checkoutCompletedEvent(t *testing.T, id string) []byte {
	return stripeEvent(t, id, "checkout.session.completed", map[string]any{
		"id":               "cs_test",
		"object":           "checkout.session",
		"customer":         "cus_123",
		"subscription":     "sub_123",
		"customer_details": map[string]any{"email": "alice@example.com"},
		"metadata": map[string]string{
			"game":      "minecraft",
			"variant":   "paper",
			"price_id":  "price_test",
			"subdomain": "alice",
		},
	})
}

// signedRequest returns a webhook POST signed with secret, the way Stripe
// would send it.
func signedRequest(body []byte, secret string) *http.Request {
	signed := webhook.GenerateTestSignedPayload(&webhook.UnsignedPayload{Payload: body, Secret: secret})
	req := httptest.NewRequest(http.MethodPost, "/api/webhook", strings.NewReader(string(body)))
	req.Header.Set("Stripe-Signature", signed.Header)
	return req
}

func serve(h *WebhookHandler, req *http.Request) *httptest.ResponseRecorder {
	rec := httptest.NewRecorder()
	h.HandleWebhook(rec, req)
	return rec
}

func TestWebhookDuplicateEventProvisionsOnce(t *testing.T) {
	h, prov, _ := newTestWebhookHandler()
	body := checkoutCompletedEvent(t, "evt_dup")

	first := serve(h, signedRequest(body, testWebhookSecret))
	second := serve(h, signedRequest(body, testWebhookSecret))

	if first.Code != http.StatusOK || second.Code != http.StatusOK {
		t.Fatalf("status codes = %d, %d; want 200, 200", first.Code, second.Code)
	}
	if n := len(prov.provisioned); n != 1 {
		t.Fatalf("ProvisionServer called %d times, want 1", n)
	}
	got := prov.provisioned[0]
	if got.CustomerEmail != "alice@example.com" || got.StripeSubID != "sub_123" || got.Game != "minecraft" {
		t.Errorf("provision request = %+v", got)
	}
}

// Example of driving a failure: keep it as a pattern for the other cases.
func TestWebhookProvisioningErrorReleasesEvent(t *testing.T) {
	h, prov, store := newTestWebhookHandler()
	prov.provisionErr = errors.New("pterodactyl down")

	rec := serve(h, signedRequest(checkoutCompletedEvent(t, "evt_fail"), testWebhookSecret))

	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500", rec.Code)
	}
	if len(store.released) != 1 || store.released[0] != "evt_fail" {
		t.Errorf("released = %v, want [evt_fail] so Stripe's retry can claim it", store.released)
	}
}

func TestWebhookRejectsBadSignatures(t *testing.T) {
	body := checkoutCompletedEvent(t, "evt_sig")
	unsigned := httptest.NewRequest(http.MethodPost, "/api/webhook", strings.NewReader(string(body)))

	cases := map[string]*http.Request{
		"wrong secret":   signedRequest(body, "whsec_wrong"),
		"missing header": unsigned,
	}
	for name, req := range cases {
		t.Run(name, func(t *testing.T) {
			h, prov, store := newTestWebhookHandler()
			rec := serve(h, req)

			if rec.Code != http.StatusUnauthorized {
				t.Errorf("status = %d, want 401", rec.Code)
			}
			if len(prov.provisioned) != 0 || len(store.claimed) != 0 {
				t.Errorf("unsigned event had effects: provisioned=%d claimed=%d", len(prov.provisioned), len(store.claimed))
			}
		})
	}
}

func TestWebhookRejectsOversizedBody(t *testing.T) {
	h, prov, _ := newTestWebhookHandler()
	body := make([]byte, maxWebhookBodyBytes+1)

	rec := serve(h, signedRequest(body, testWebhookSecret))

	if rec.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", rec.Code)
	}
	if len(prov.provisioned) != 0 {
		t.Error("oversized body was provisioned")
	}
}

// invoiceEvent builds an invoice event for the given subscription.
func invoiceEvent(t *testing.T, id, eventType, subID string, extra map[string]any) []byte {
	invoice := map[string]any{"id": "in_" + id, "object": "invoice", "customer": "cus_1"}
	if subID != "" {
		invoice["parent"] = map[string]any{
			"type":                 "subscription_details",
			"subscription_details": map[string]any{"subscription": subID},
		}
	}
	for k, v := range extra {
		invoice[k] = v
	}
	return stripeEvent(t, id, eventType, invoice)
}

func subscriptionEvent(t *testing.T, id, eventType, subID, status string) []byte {
	return stripeEvent(t, id, eventType, map[string]any{"id": subID, "object": "subscription", "status": status})
}

func TestWebhookRoutesEventsByType(t *testing.T) {
	cases := []struct {
		name      string
		body      func(t *testing.T) []byte
		wantCalls []string
	}{
		{
			name:      "checkout provisions",
			body:      func(t *testing.T) []byte { return checkoutCompletedEvent(t, "evt_co") },
			wantCalls: []string{"provision alice@example.com"},
		},
		{
			name: "checkout without an email does nothing",
			body: func(t *testing.T) []byte {
				return stripeEvent(t, "evt_noemail", "checkout.session.completed",
					map[string]any{"id": "cs_2", "object": "checkout.session"})
			},
		},
		{
			name: "subscription deleted suspends as cancelled",
			body: func(t *testing.T) []byte {
				return subscriptionEvent(t, "evt_del", "customer.subscription.deleted", "sub_gone", "canceled")
			},
			wantCalls: []string{"suspend sub_gone cancelled"},
		},
		{
			name: "payment failed records the failure without suspending",
			body: func(t *testing.T) []byte {
				return invoiceEvent(t, "evt_pf", "invoice.payment_failed", "sub_1", nil)
			},
			wantCalls: []string{"payment_failed sub_1"},
		},
		{
			name: "payment failed without a subscription does nothing",
			body: func(t *testing.T) []byte {
				return invoiceEvent(t, "evt_pf2", "invoice.payment_failed", "", nil)
			},
		},
		{
			name: "retries exhausted (unpaid) suspends for non-payment",
			body: func(t *testing.T) []byte {
				return subscriptionEvent(t, "evt_unpaid", "customer.subscription.updated", "sub_1", "unpaid")
			},
			wantCalls: []string{"suspend sub_1 non_payment"},
		},
		{
			name: "retries exhausted (canceled) suspends as cancelled",
			body: func(t *testing.T) []byte {
				return subscriptionEvent(t, "evt_canc", "customer.subscription.updated", "sub_1", "canceled")
			},
			wantCalls: []string{"suspend sub_1 cancelled"},
		},
		{
			name: "subscription back to active restores",
			body: func(t *testing.T) []byte {
				return subscriptionEvent(t, "evt_active", "customer.subscription.updated", "sub_1", "active")
			},
			wantCalls: []string{"restore sub_1"},
		},
		{
			name: "past_due update is left to invoice.payment_failed",
			body: func(t *testing.T) []byte {
				return subscriptionEvent(t, "evt_pd", "customer.subscription.updated", "sub_1", "past_due")
			},
		},
		{
			name: "invoice paid restores",
			body: func(t *testing.T) []byte {
				return invoiceEvent(t, "evt_paid", "invoice.paid", "sub_1", nil)
			},
			wantCalls: []string{"restore sub_1"},
		},
		{
			name: "one-off invoice paid does nothing",
			body: func(t *testing.T) []byte {
				return invoiceEvent(t, "evt_paid2", "invoice.paid", "", nil)
			},
		},
		{
			name: "unknown event type is acknowledged",
			body: func(t *testing.T) []byte {
				return stripeEvent(t, "evt_other", "customer.created",
					map[string]any{"id": "cus_1", "object": "customer"})
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h, prov, _ := newTestWebhookHandler()
			rec := serve(h, signedRequest(tc.body(t), testWebhookSecret))

			if rec.Code != http.StatusOK {
				t.Fatalf("status = %d, want 200", rec.Code)
			}
			if strings.Join(prov.calls, ", ") != strings.Join(tc.wantCalls, ", ") {
				t.Errorf("calls = %v, want %v", prov.calls, tc.wantCalls)
			}
		})
	}
}

func TestWebhookPaymentFailedPassesInvoiceDetails(t *testing.T) {
	h, prov, _ := newTestWebhookHandler()
	body := invoiceEvent(t, "evt_pfd", "invoice.payment_failed", "sub_1", map[string]any{
		"customer_email":     "alice@example.com",
		"hosted_invoice_url": "https://invoice.stripe.test/i/1",
		"attempt_count":      2,
	})

	if rec := serve(h, signedRequest(body, testWebhookSecret)); rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	want := provisioner.PaymentFailure{
		StripeSubID:   "sub_1",
		CustomerEmail: "alice@example.com",
		InvoiceURL:    "https://invoice.stripe.test/i/1",
		AttemptCount:  2,
	}
	if len(prov.paymentFailed) != 1 || prov.paymentFailed[0] != want {
		t.Errorf("payment failures = %+v, want %+v", prov.paymentFailed, want)
	}
}

func TestWebhookSuspendErrorReleasesEvent(t *testing.T) {
	h, prov, store := newTestWebhookHandler()
	prov.suspendErr = errors.New("db down")
	body := stripeEvent(t, "evt_del_fail", "customer.subscription.deleted",
		map[string]any{"id": "sub_1", "object": "subscription"})

	rec := serve(h, signedRequest(body, testWebhookSecret))

	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500", rec.Code)
	}
	if store.claimed["evt_del_fail"] {
		t.Error("event still claimed after failure; Stripe's retry would be skipped")
	}
}

func TestWebhookClaimErrorReturns500(t *testing.T) {
	h, prov, store := newTestWebhookHandler()
	store.claimErr = errors.New("db down")

	rec := serve(h, signedRequest(checkoutCompletedEvent(t, "evt_claim"), testWebhookSecret))

	if rec.Code != http.StatusInternalServerError {
		t.Errorf("status = %d, want 500", rec.Code)
	}
	if len(prov.provisioned) != 0 {
		t.Error("provisioned without a successful claim")
	}
}

func TestWebhookPassesCheckoutFieldsToProvisioner(t *testing.T) {
	h, prov, _ := newTestWebhookHandler()
	body := stripeEvent(t, "evt_fields", "checkout.session.completed", map[string]any{
		"id":               "cs_3",
		"object":           "checkout.session",
		"customer":         "cus_9",
		"subscription":     "sub_9",
		"customer_details": map[string]any{"email": "bob@example.com"},
		"metadata": map[string]string{
			"game": "rust", "variant": "oxide", "version": "", "subdomain": "bob", "price_id": "price_8gb",
		},
		"custom_fields": []map[string]any{
			{"key": "username", "type": "text", "text": map[string]any{"value": "  bobby  "}},
		},
	})

	if rec := serve(h, signedRequest(body, testWebhookSecret)); rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	want := provisioner.Request{
		CustomerEmail:     "bob@example.com",
		Game:              "rust",
		Variant:           "oxide",
		Subdomain:         "bob",
		PriceID:           "price_8gb",
		StripeSubID:       "sub_9",
		StripeCustomerID:  "cus_9",
		RequestedUsername: "bobby",
	}
	if len(prov.provisioned) != 1 || prov.provisioned[0] != want {
		t.Errorf("provision request = %+v, want %+v", prov.provisioned, want)
	}
}
