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
	mu           sync.Mutex
	provisioned  []provisioner.Request
	suspended    []string
	provisionErr error
	suspendErr   error
}

func (f *fakeProvisioner) ProvisionServer(_ context.Context, req provisioner.Request) (*provisioner.ProvisionResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.provisionErr != nil {
		return nil, f.provisionErr
	}
	f.provisioned = append(f.provisioned, req)
	return &provisioner.ProvisionResult{Username: "kuu_test", ServerName: "test-server"}, nil
}

func (f *fakeProvisioner) SuspendServer(_ context.Context, stripeSubID string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.suspendErr != nil {
		return f.suspendErr
	}
	f.suspended = append(f.suspended, stripeSubID)
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

func TestWebhookRoutesEventsByType(t *testing.T) {
	cases := []struct {
		name          string
		body          func(t *testing.T) []byte
		wantSuspended []string
		wantProvision int
	}{
		{
			name: "subscription deleted suspends",
			body: func(t *testing.T) []byte {
				return stripeEvent(t, "evt_del", "customer.subscription.deleted",
					map[string]any{"id": "sub_gone", "object": "subscription"})
			},
			wantSuspended: []string{"sub_gone"},
		},
		{
			name: "payment failed suspends the invoice's subscription",
			body: func(t *testing.T) []byte {
				return stripeEvent(t, "evt_pf", "invoice.payment_failed", map[string]any{
					"id": "in_1", "object": "invoice", "customer": "cus_1",
					"parent": map[string]any{
						"type":                 "subscription_details",
						"subscription_details": map[string]any{"subscription": "sub_unpaid"},
					},
				})
			},
			wantSuspended: []string{"sub_unpaid"},
		},
		{
			name: "payment failed without a subscription does nothing",
			body: func(t *testing.T) []byte {
				return stripeEvent(t, "evt_pf2", "invoice.payment_failed",
					map[string]any{"id": "in_2", "object": "invoice"})
			},
		},
		{
			name: "checkout without an email does nothing",
			body: func(t *testing.T) []byte {
				return stripeEvent(t, "evt_noemail", "checkout.session.completed",
					map[string]any{"id": "cs_2", "object": "checkout.session"})
			},
		},
		{
			name: "unknown event type is acknowledged",
			body: func(t *testing.T) []byte {
				return stripeEvent(t, "evt_other", "customer.created",
					map[string]any{"id": "cus_1", "object": "customer"})
			},
		},
		{
			name:          "checkout provisions",
			body:          func(t *testing.T) []byte { return checkoutCompletedEvent(t, "evt_co") },
			wantProvision: 1,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h, prov, _ := newTestWebhookHandler()
			rec := serve(h, signedRequest(tc.body(t), testWebhookSecret))

			if rec.Code != http.StatusOK {
				t.Fatalf("status = %d, want 200", rec.Code)
			}
			if strings.Join(prov.suspended, ",") != strings.Join(tc.wantSuspended, ",") {
				t.Errorf("suspended = %v, want %v", prov.suspended, tc.wantSuspended)
			}
			if len(prov.provisioned) != tc.wantProvision {
				t.Errorf("provisioned %d times, want %d", len(prov.provisioned), tc.wantProvision)
			}
		})
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
