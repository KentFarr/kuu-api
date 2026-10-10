package handlers

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"net/http"
	"net/url"
	"strconv"
	"time"

	"github.com/stripe/stripe-go/v82"
	"github.com/stripe/stripe-go/v82/billingportal/session"

	"github.com/KentFarr/kuu-api/internal/logging"
)

// portalLinkTTL is how long an emailed "manage subscription" link works.
const portalLinkTTL = 24 * time.Hour

var (
	errPortalLinkInvalid = errors.New("invalid portal link")
	errPortalLinkExpired = errors.New("portal link expired")
)

// Stripe billing-portal sessions are short-lived, so emailing one directly
// risks a dead link. Instead the email carries a signed link back to us
// (customer ID + expiry, HMAC-SHA256), and a fresh portal session is
// created only when the customer clicks it.

func signPortalLink(secret []byte, stripeCustomerID string, expires time.Time) string {
	mac := hmac.New(sha256.New, secret)
	mac.Write([]byte(stripeCustomerID + "|" + strconv.FormatInt(expires.Unix(), 10)))
	return base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
}

// portalLinkURL builds the emailed link. base is the configured public API
// URL, never the request's Host header, which a caller could forge to get
// a phishing link sent from our address.
func (h *SupportHandler) portalLinkURL(stripeCustomerID string) string {
	expires := h.now().Add(portalLinkTTL)
	q := url.Values{
		"c": {stripeCustomerID},
		"e": {strconv.FormatInt(expires.Unix(), 10)},
		"s": {signPortalLink(h.LinkSecret, stripeCustomerID, expires)},
	}
	return h.PublicURL + "/api/support/portal?" + q.Encode()
}

// verifyPortalLink returns the Stripe customer ID from a valid, unexpired
// link.
func (h *SupportHandler) verifyPortalLink(q url.Values) (string, error) {
	customerID, sig := q.Get("c"), q.Get("s")
	expUnix, err := strconv.ParseInt(q.Get("e"), 10, 64)
	if customerID == "" || sig == "" || err != nil {
		return "", errPortalLinkInvalid
	}
	expires := time.Unix(expUnix, 0)
	want := signPortalLink(h.LinkSecret, customerID, expires)
	if !hmac.Equal([]byte(sig), []byte(want)) {
		return "", errPortalLinkInvalid
	}
	if h.now().After(expires) {
		return "", errPortalLinkExpired
	}
	return customerID, nil
}

func (h *SupportHandler) now() time.Time {
	if h.Now != nil {
		return h.Now()
	}
	return time.Now()
}

// HandlePortalRedirect is the target of the emailed link: it checks the
// signature and expiry, then sends the browser to a freshly created Stripe
// billing-portal session.
func (h *SupportHandler) HandlePortalRedirect(w http.ResponseWriter, r *http.Request) {
	logger := logging.FromContext(r.Context())

	customerID, err := h.verifyPortalLink(r.URL.Query())
	switch {
	case errors.Is(err, errPortalLinkExpired):
		http.Error(w, "This link has expired. Request a new one from kuu.gg.", http.StatusGone)
		return
	case err != nil:
		logger.Warn("invalid portal link", "err", err)
		http.Error(w, "This link is not valid.", http.StatusForbidden)
		return
	}

	ps, err := session.New(&stripe.BillingPortalSessionParams{
		Customer:  stripe.String(customerID),
		ReturnURL: stripe.String(h.ReturnURL),
	})
	if err != nil {
		logger.Error("failed to create billing portal session", "err", err)
		http.Error(w, "Couldn't open the billing portal. Please try again.", http.StatusBadGateway)
		return
	}
	http.Redirect(w, r, ps.URL, http.StatusSeeOther)
}
