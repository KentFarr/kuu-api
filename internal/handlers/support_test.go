package handlers

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

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

var testLinkSecret = []byte("test-link-secret")

func newPortalHandler(queue *fakeEnqueuer, now time.Time) *SupportHandler {
	return &SupportHandler{
		DB:         &fakeSupportStore{customer: existingCustomer()},
		EmailQueue: queue,
		ReturnURL:  "https://kuu.test",
		PublicURL:  "https://api.kuu.test",
		LinkSecret: testLinkSecret,
		Now:        func() time.Time { return now },
	}
}

// emailedPortalLink requests a manage-subscription email and returns the
// link inside it.
func emailedPortalLink(t *testing.T, h *SupportHandler, queue *fakeEnqueuer) *url.URL {
	t.Helper()
	if rec := supportPost(h.HandleManageSubscription, `{"email":"alice@example.com"}`); rec.Code != http.StatusOK {
		t.Fatalf("manage: status = %d, want 200", rec.Code)
	}
	if len(queue.sent) != 1 {
		t.Fatalf("emails = %d, want 1", len(queue.sent))
	}
	raw := regexp.MustCompile(`https://\S+`).FindString(queue.sent[0].Body)
	u, err := url.Parse(raw)
	if err != nil || raw == "" {
		t.Fatalf("no link in email body:\n%s", queue.sent[0].Body)
	}
	return u
}

func portalGet(h *SupportHandler, target string) *httptest.ResponseRecorder {
	rec := httptest.NewRecorder()
	h.HandlePortalRedirect(rec, httptest.NewRequest(http.MethodGet, target, nil))
	return rec
}

func TestManageSubscriptionEmailsSignedLinkThatOpensPortal(t *testing.T) {
	now := time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC)
	queue := &fakeEnqueuer{}
	h := newPortalHandler(queue, now)

	link := emailedPortalLink(t, h, queue)
	if link.Host != "api.kuu.test" || link.Path != "/api/support/portal" {
		t.Fatalf("link = %s, want the configured public URL", link)
	}

	// Clicking it an hour later creates a fresh portal session.
	stripeAPI := newFakeStripe(t, stripeReplies(http.StatusOK,
		`{"id": "bps_1", "object": "billing_portal.session", "url": "https://billing.stripe.test/p/1"}`))
	h.Now = func() time.Time { return now.Add(time.Hour) }
	rec := portalGet(h, link.RequestURI())

	if rec.Code != http.StatusSeeOther || rec.Header().Get("Location") != "https://billing.stripe.test/p/1" {
		t.Fatalf("redirect = %d %q, want 303 to the portal session", rec.Code, rec.Header().Get("Location"))
	}
	if form := stripeAPI.calls[0].Form; form.Get("customer") != "cus_1" || form.Get("return_url") != "https://kuu.test" {
		t.Errorf("portal session params = %v", form)
	}
}

func TestManageSubscriptionIgnoresHostHeader(t *testing.T) {
	queue := &fakeEnqueuer{}
	h := newPortalHandler(queue, time.Now())
	req := httptest.NewRequest(http.MethodPost, "/api/support/manage", strings.NewReader(`{"email":"alice@example.com"}`))
	req.Host = "evil.example"
	h.HandleManageSubscription(httptest.NewRecorder(), req)

	if len(queue.sent) != 1 || strings.Contains(queue.sent[0].Body, "evil.example") {
		t.Errorf("emailed link must use the configured URL, not the Host header:\n%+v", queue.sent)
	}
}

func TestPortalRedirectRejectsBadLinks(t *testing.T) {
	now := time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC)
	queue := &fakeEnqueuer{}
	h := newPortalHandler(queue, now)
	link := emailedPortalLink(t, h, queue)
	stripeAPI := newFakeStripe(t, stripeReplies(http.StatusOK, `{"id": "bps_1", "url": "https://x"}`))

	tampered := link.Query()
	tampered.Set("c", "cus_someone_else")
	extended := link.Query()
	extended.Set("e", strconv.FormatInt(now.Add(365*24*time.Hour).Unix(), 10))
	otherKey := link.Query()
	otherKey.Set("s", signPortalLink([]byte("wrong"), "cus_1", now.Add(portalLinkTTL)))

	cases := []struct {
		name   string
		target string
		at     time.Time
		want   int
	}{
		{"no parameters", "/api/support/portal", now, http.StatusForbidden},
		{"different customer", "/api/support/portal?" + tampered.Encode(), now, http.StatusForbidden},
		{"extended expiry", "/api/support/portal?" + extended.Encode(), now, http.StatusForbidden},
		{"signed with another key", "/api/support/portal?" + otherKey.Encode(), now, http.StatusForbidden},
		{"expired", link.RequestURI(), now.Add(portalLinkTTL + time.Minute), http.StatusGone},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h.Now = func() time.Time { return tc.at }
			if rec := portalGet(h, tc.target); rec.Code != tc.want {
				t.Errorf("status = %d, want %d", rec.Code, tc.want)
			}
		})
	}
	if len(stripeAPI.calls) != 0 {
		t.Errorf("rejected links reached Stripe %d times", len(stripeAPI.calls))
	}
}

func TestPortalRedirectStripeErrorReturns502(t *testing.T) {
	now := time.Now()
	queue := &fakeEnqueuer{}
	h := newPortalHandler(queue, now)
	link := emailedPortalLink(t, h, queue)
	newFakeStripe(t, stripeReplies(http.StatusInternalServerError, stripeErrorBody))

	if rec := portalGet(h, link.RequestURI()); rec.Code != http.StatusBadGateway {
		t.Errorf("status = %d, want 502", rec.Code)
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
	t.Run("enqueue fails", func(t *testing.T) {
		h := newPortalHandler(&fakeEnqueuer{err: errors.New("db")}, time.Now())
		if rec := supportPost(h.HandleManageSubscription, `{"email":"a@b.c"}`); rec.Code != http.StatusInternalServerError {
			t.Errorf("status = %d, want 500", rec.Code)
		}
	})
}
