package handlers

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/KentFarr/kuu-api/internal/database"
	"github.com/KentFarr/kuu-api/internal/email"
)

const supportOKMessage = "If an active subscription exists"

type fakeSupportStore struct {
	customer *database.Customer
	server   *database.Server
	err      error
}

func (f *fakeSupportStore) GetCustomerByEmail(string) (*database.Customer, error) {
	return f.customer, f.err
}

func (f *fakeSupportStore) GetLatestActiveServerByCustomerID(int) (*database.Server, error) {
	return f.server, nil
}

type fakeSupportPtero struct {
	fqdnErr error
}

func (f *fakeSupportPtero) GetNodeFQDN(context.Context, int) (string, error) {
	return "node7.test", f.fqdnErr
}

type fakeEnqueuer struct {
	sent []sentMail
	err  error
}

type sentMail struct{ To, Subject, Body string }

func (f *fakeEnqueuer) Enqueue(to, subject, body string) error {
	if f.err != nil {
		return f.err
	}
	f.sent = append(f.sent, sentMail{to, subject, body})
	return nil
}

func ptr[T any](v T) *T { return &v }

func existingCustomer() *database.Customer {
	return &database.Customer{
		ID:                  1,
		Email:               "alice@example.com",
		PterodactylUserID:   ptr(42),
		PterodactylUsername: ptr("alice"),
		StripeCustomerID:    ptr("cus_1"),
	}
}

func supportPost(handler http.HandlerFunc, body string) *httptest.ResponseRecorder {
	rec := httptest.NewRecorder()
	handler(rec, httptest.NewRequest(http.MethodPost, "/api/support", strings.NewReader(body)))
	return rec
}

func TestResendWelcomeEmailsServerDetails(t *testing.T) {
	ptero := &fakeSupportPtero{}
	queue := &fakeEnqueuer{}
	h := &SupportHandler{
		DB:          &fakeSupportStore{customer: existingCustomer(), server: &database.Server{ServerName: "mc-1", Game: "minecraft", Port: 25565}},
		PteroClient: ptero,
		EmailQueue:  queue,
	}

	rec := supportPost(h.HandleResendWelcome, `{"email":"  Alice@Example.com "}`)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if len(queue.sent) != 1 || queue.sent[0].To != "alice@example.com" {
		t.Fatalf("emails = %+v, want one to the normalized address", queue.sent)
	}
	body := queue.sent[0].Body
	if !strings.Contains(body, "node7.test") || !strings.Contains(body, email.ForgotPasswordURL) {
		t.Errorf("email should contain the node address and the panel's reset link:\n%s", body)
	}
	if strings.Contains(body, "Password:") {
		t.Errorf("email contains a password:\n%s", body)
	}
}

// Unknown customers must get the same 200 as real ones, so the endpoint
// can't be used to discover which emails have accounts.
func TestSupportEndpointsDoNotRevealUnknownEmails(t *testing.T) {
	queue := &fakeEnqueuer{}
	h := &SupportHandler{DB: &fakeSupportStore{}, PteroClient: &fakeSupportPtero{}, EmailQueue: queue}

	for name, handler := range map[string]http.HandlerFunc{
		"resend-welcome": h.HandleResendWelcome,
		"manage":         h.HandleManageSubscription,
	} {
		t.Run(name, func(t *testing.T) {
			rec := supportPost(handler, `{"email":"nobody@example.com"}`)
			if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), supportOKMessage) {
				t.Errorf("response = %d %s, want the generic 200", rec.Code, rec.Body.String())
			}
		})
	}
	if len(queue.sent) != 0 {
		t.Errorf("emailed an unknown address: %+v", queue.sent)
	}
}

func TestResendWelcomeErrors(t *testing.T) {
	cases := []struct {
		name  string
		body  string
		h     *SupportHandler
		want  int
		email bool
	}{
		{"bad JSON", `{`, &SupportHandler{}, http.StatusBadRequest, false},
		{"empty email", `{"email":"  "}`, &SupportHandler{}, http.StatusBadRequest, false},
		{"lookup fails", `{"email":"a@b.c"}`, &SupportHandler{DB: &fakeSupportStore{err: errors.New("db")}}, http.StatusInternalServerError, false},
		{"no active server", `{"email":"a@b.c"}`, &SupportHandler{DB: &fakeSupportStore{customer: existingCustomer()}}, http.StatusOK, false},
		{"node lookup fails uses fallback", `{"email":"a@b.c"}`, &SupportHandler{
			DB:          &fakeSupportStore{customer: existingCustomer(), server: &database.Server{}},
			PteroClient: &fakeSupportPtero{fqdnErr: errors.New("panel down")},
		}, http.StatusOK, true},
		{"enqueue fails", `{"email":"a@b.c"}`, &SupportHandler{
			DB:          &fakeSupportStore{customer: existingCustomer(), server: &database.Server{}},
			PteroClient: &fakeSupportPtero{},
			EmailQueue:  &fakeEnqueuer{err: errors.New("db")},
		}, http.StatusInternalServerError, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			queue := &fakeEnqueuer{}
			if tc.h.EmailQueue == nil {
				tc.h.EmailQueue = queue
			}
			rec := supportPost(tc.h.HandleResendWelcome, tc.body)
			if rec.Code != tc.want {
				t.Errorf("status = %d, want %d", rec.Code, tc.want)
			}
			if got := len(queue.sent) == 1; got != tc.email {
				t.Errorf("email sent = %v, want %v", got, tc.email)
			}
		})
	}
}

func TestManageSubscriptionEmailsPortalLink(t *testing.T) {
	stripeAPI := newFakeStripe(t, stripeReplies(http.StatusOK,
		`{"id": "bps_1", "object": "billing_portal.session", "url": "https://billing.stripe.test/p/1"}`))
	queue := &fakeEnqueuer{}
	h := &SupportHandler{DB: &fakeSupportStore{customer: existingCustomer()}, EmailQueue: queue, ReturnURL: "https://kuu.test"}

	rec := supportPost(h.HandleManageSubscription, `{"email":"alice@example.com"}`)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if form := stripeAPI.calls[0].Form; form.Get("customer") != "cus_1" || form.Get("return_url") != "https://kuu.test" {
		t.Errorf("portal session params = %v", form)
	}
	if len(queue.sent) != 1 || !strings.Contains(queue.sent[0].Body, "https://billing.stripe.test/p/1") {
		t.Errorf("emails = %+v, want one containing the portal link", queue.sent)
	}
}

func TestManageSubscriptionErrors(t *testing.T) {
	t.Run("bad JSON", func(t *testing.T) {
		if rec := supportPost((&SupportHandler{}).HandleManageSubscription, `{`); rec.Code != http.StatusBadRequest {
			t.Errorf("status = %d, want 400", rec.Code)
		}
	})
	t.Run("empty email", func(t *testing.T) {
		if rec := supportPost((&SupportHandler{}).HandleManageSubscription, `{"email":""}`); rec.Code != http.StatusBadRequest {
			t.Errorf("status = %d, want 400", rec.Code)
		}
	})
	t.Run("lookup fails", func(t *testing.T) {
		h := &SupportHandler{DB: &fakeSupportStore{err: errors.New("db")}}
		if rec := supportPost(h.HandleManageSubscription, `{"email":"a@b.c"}`); rec.Code != http.StatusInternalServerError {
			t.Errorf("status = %d, want 500", rec.Code)
		}
	})
	t.Run("stripe fails", func(t *testing.T) {
		newFakeStripe(t, stripeReplies(http.StatusInternalServerError, stripeErrorBody))
		h := &SupportHandler{DB: &fakeSupportStore{customer: existingCustomer()}, EmailQueue: &fakeEnqueuer{}}
		if rec := supportPost(h.HandleManageSubscription, `{"email":"a@b.c"}`); rec.Code != http.StatusInternalServerError {
			t.Errorf("status = %d, want 500", rec.Code)
		}
	})
	t.Run("enqueue fails", func(t *testing.T) {
		newFakeStripe(t, stripeReplies(http.StatusOK, `{"id": "bps_1", "url": "https://x"}`))
		h := &SupportHandler{DB: &fakeSupportStore{customer: existingCustomer()}, EmailQueue: &fakeEnqueuer{err: errors.New("db")}}
		if rec := supportPost(h.HandleManageSubscription, `{"email":"a@b.c"}`); rec.Code != http.StatusInternalServerError {
			t.Errorf("status = %d, want 500", rec.Code)
		}
	})
}
