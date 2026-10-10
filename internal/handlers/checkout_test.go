package handlers

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func setTestPrices(t *testing.T) {
	t.Helper()
	t.Setenv("STRIPE_PRICE_2GB", "price_2gb")
	t.Setenv("STRIPE_PRICE_4GB", "price_4gb")
	t.Setenv("STRIPE_PRICE_8GB", "price_8gb")
}

func postCheckout(h *CheckoutHandler, body string) *httptest.ResponseRecorder {
	rec := httptest.NewRecorder()
	h.HandleCheckout(rec, httptest.NewRequest(http.MethodPost, "/api/checkout", strings.NewReader(body)))
	return rec
}

func TestCheckoutRejectsInvalidRequests(t *testing.T) {
	setTestPrices(t)
	cases := map[string]string{
		"malformed JSON":             `{`,
		"unknown field":              `{"game":"minecraft","price_id":"price_2gb","extra":1}`,
		"missing game":               `{"price_id":"price_2gb"}`,
		"missing price":              `{"game":"minecraft"}`,
		"unknown game":               `{"game":"tetris","price_id":"price_2gb"}`,
		"unknown variant":            `{"game":"minecraft","variant":"bukkit","price_id":"price_2gb"}`,
		"version on versionless":     `{"game":"rust","version":"1.0","price_id":"price_8gb"}`,
		"version with bad chars":     `{"game":"minecraft","version":"1.21;rm -rf","price_id":"price_2gb"}`,
		"unknown price":              `{"game":"minecraft","price_id":"price_nope"}`,
		"subdomain below 8GB":        `{"game":"minecraft","subdomain":"alice","price_id":"price_4gb"}`,
		"subdomain not DNS-safe":     `{"game":"minecraft","subdomain":"al ice","price_id":"price_8gb"}`,
		"rust below 8GB":             `{"game":"rust","price_id":"price_4gb"}`,
		"gmod below 4GB":             `{"game":"gmod","price_id":"price_2gb"}`,
		"body over 4 KiB size limit": `{"game":"` + strings.Repeat("a", 5000) + `"}`,
	}
	for name, body := range cases {
		t.Run(name, func(t *testing.T) {
			stripeAPI := newFakeStripe(t, stripeReplies(http.StatusOK, `{}`))
			rec := postCheckout(&CheckoutHandler{}, body)

			if rec.Code != http.StatusBadRequest {
				t.Errorf("status = %d (%s), want 400", rec.Code, strings.TrimSpace(rec.Body.String()))
			}
			if len(stripeAPI.calls) != 0 {
				t.Error("invalid request reached Stripe")
			}
		})
	}
}

func TestCheckoutCreatesSessionWithMetadata(t *testing.T) {
	setTestPrices(t)
	stripeAPI := newFakeStripe(t, stripeReplies(http.StatusOK,
		`{"id": "cs_1", "object": "checkout.session", "url": "https://checkout.stripe.test/cs_1"}`))
	h := &CheckoutHandler{SuccessURL: "https://kuu.test/ok", CancelURL: "https://kuu.test/cancel", StripeCouponID: "coupon_first"}

	rec := postCheckout(h, `{"game":"minecraft","version":"1.21.1","subdomain":"Alice","price_id":"price_8gb"}`)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d (%s), want 200", rec.Code, rec.Body.String())
	}
	var resp CheckoutResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil || resp.CheckoutURL != "https://checkout.stripe.test/cs_1" {
		t.Fatalf("response = %s (%v), want the session URL", rec.Body.String(), err)
	}

	if len(stripeAPI.calls) != 1 {
		t.Fatalf("stripe calls = %d, want 1", len(stripeAPI.calls))
	}
	form := stripeAPI.calls[0].Form
	want := map[string]string{
		"mode":                  "subscription",
		"line_items[0][price]":  "price_8gb",
		"metadata[game]":        "minecraft",
		"metadata[variant]":     "paper", // default variant written back
		"metadata[version]":     "1.21.1",
		"metadata[subdomain]":   "alice", // lower-cased
		"metadata[price_id]":    "price_8gb",
		"discounts[0][coupon]":  "coupon_first",
		"custom_fields[0][key]": "username",
	}
	for k, v := range want {
		if got := form.Get(k); got != v {
			t.Errorf("%s = %q, want %q", k, got, v)
		}
	}
}

func TestCheckoutSkipsPlaceholderCoupon(t *testing.T) {
	setTestPrices(t)
	stripeAPI := newFakeStripe(t, stripeReplies(http.StatusOK, `{"id": "cs_1", "url": "https://x"}`))

	rec := postCheckout(&CheckoutHandler{StripeCouponID: "promo_"}, `{"game":"minecraft","price_id":"price_2gb"}`)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if got := stripeAPI.calls[0].Form.Get("discounts[0][coupon]"); got != "" {
		t.Errorf("placeholder coupon sent to Stripe: %q", got)
	}
}

func TestCheckoutStripeErrorReturns500(t *testing.T) {
	setTestPrices(t)
	newFakeStripe(t, stripeReplies(http.StatusInternalServerError, stripeErrorBody))

	rec := postCheckout(&CheckoutHandler{}, `{"game":"minecraft","price_id":"price_2gb"}`)

	if rec.Code != http.StatusInternalServerError {
		t.Errorf("status = %d, want 500", rec.Code)
	}
}
