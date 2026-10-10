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
}

func newFakeEventStore() *fakeEventStore {
	return &fakeEventStore{claimed: map[string]bool{}}
}

func (f *fakeEventStore) ClaimWebhookEvent(eventID string) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
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
