package handlers

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"sync"
	"testing"

	"github.com/stripe/stripe-go/v82"
)

// fakeStripe points stripe-go at a local server for the rest of the test.
// respond writes the reply for each API call; every request's form is
// recorded in calls. stripe-go's backend is global, so tests that use this
// must not run in parallel.
type fakeStripe struct {
	mu    sync.Mutex
	calls []stripeCall
}

type stripeCall struct {
	Path string
	Form url.Values
}

func newFakeStripe(t *testing.T, respond http.HandlerFunc) *fakeStripe {
	t.Helper()
	f := &fakeStripe{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		f.mu.Lock()
		f.calls = append(f.calls, stripeCall{Path: r.URL.Path, Form: r.PostForm})
		f.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		respond(w, r)
	}))
	t.Cleanup(srv.Close)

	origBackend := stripe.GetBackend(stripe.APIBackend)
	origKey := stripe.Key
	stripe.Key = "sk_test_fake"
	stripe.SetBackend(stripe.APIBackend, stripe.GetBackendWithConfig(stripe.APIBackend, &stripe.BackendConfig{
		URL:               stripe.String(srv.URL),
		MaxNetworkRetries: stripe.Int64(0),
		LeveledLogger:     &stripe.LeveledLogger{Level: stripe.LevelNull},
	}))
	t.Cleanup(func() {
		stripe.SetBackend(stripe.APIBackend, origBackend)
		stripe.Key = origKey
	})
	return f
}

// stripeReplies returns a handler that answers every call with body.
func stripeReplies(status int, body string) http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}
}

const stripeErrorBody = `{"error": {"type": "api_error", "message": "boom"}}`
