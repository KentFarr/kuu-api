package provisioner

import (
	"context"
	"errors"
	"strings"
	"testing"
)

func validRequest() Request {
	return Request{
		CustomerEmail:    "alice@example.com",
		Game:             "minecraft",
		Subdomain:        "alice",
		PriceID:          testPriceID,
		StripeSubID:      "sub_123",
		StripeCustomerID: "cus_123",
	}
}

func TestProvisionServerHappyPath(t *testing.T) {
	p, ptero, store, dns, queue := newTestProvisioner(t)

	res, err := p.ProvisionServer(context.Background(), validRequest())
	if err != nil {
		t.Fatalf("ProvisionServer: %v", err)
	}

	// Pterodactyl: one new user and one server sized for the 2GB plan.
	if len(ptero.users) != 1 {
		t.Fatalf("pterodactyl users = %d, want 1", len(ptero.users))
	}
	srv, ok := ptero.servers[res.ServerID]
	if !ok {
		t.Fatalf("pterodactyl server %d not created", res.ServerID)
	}
	if srv.UserID != res.UserID || srv.RAM != 2048 || srv.AllocationID != 500 {
		t.Errorf("server params = %+v, want owner %d, RAM 2048, allocation 500", srv, res.UserID)
	}

	// Database: customer (with Stripe ID), subscription and server rows.
	if len(store.customers) != 1 || len(store.subscriptions) != 1 || len(store.servers) != 1 {
		t.Fatalf("rows: customers=%d subscriptions=%d servers=%d, want 1 each",
			len(store.customers), len(store.subscriptions), len(store.servers))
	}
	for _, c := range store.customers {
		if c.StripeCustomerID == nil || *c.StripeCustomerID != "cus_123" {
			t.Errorf("customer stripe ID = %v, want cus_123", c.StripeCustomerID)
		}
	}
	for _, s := range store.subscriptions {
		if s.StripeSubscriptionID != "sub_123" || s.Status != "active" {
			t.Errorf("subscription = %+v, want sub_123 active", s)
		}
	}
	for _, s := range store.servers {
		if s.PterodactylServerID != res.ServerID || s.Port != 25565 || s.Variant.String != "paper" {
			t.Errorf("server row = %+v, want ptero ID %d, port 25565, variant paper", s, res.ServerID)
		}
	}

	// DNS points the subdomain at the chosen node.
	if got := dns.created["alice"]; got != "node1.test" {
		t.Errorf("CNAME alice -> %q, want node1.test", got)
	}

	// Welcome email goes to the customer and uses the subdomain address.
	if len(queue.sent) != 1 {
		t.Fatalf("emails queued = %d, want 1", len(queue.sent))
	}
	if e := queue.sent[0]; e.To != "alice@example.com" || !strings.Contains(e.Body, "alice.kuu.gg") {
		t.Errorf("welcome email = %+v, want to alice@example.com mentioning alice.kuu.gg", e)
	}

	if !res.NewUser || res.PlanName != "2GB" || res.Variant != "paper" {
		t.Errorf("result = %+v, want new user, plan 2GB, variant paper", res)
	}
}

// TestProvisionServerRollsBackOnFailure is the most important test in the
// repo: whatever step fails, nothing created before it may be left behind,
// or a customer could be charged for a half-built server (or leak an orphan
// Pterodactyl server we pay for).
//
// Each case injects a failure at one step. Add a case per failure point.
func TestProvisionServerRollsBackOnFailure(t *testing.T) {
	cases := []struct {
		name   string
		inject func(ptero *fakePtero, store *fakeStore)
	}{
		{
			// The last step, so every earlier cleanup must run.
			name:   "server row insert fails",
			inject: func(_ *fakePtero, store *fakeStore) { store.fail["CreateServer"] = true },
		},
		// TODO: pterodactyl user creation fails  (ptero.fail["CreateUser"])
		// TODO: subscription insert fails        (store.fail["CreateSubscription"])
		// TODO: pterodactyl server creation fails (ptero.fail["CreateServer"])
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p, ptero, store, dns, queue := newTestProvisioner(t)
			tc.inject(ptero, store)

			_, err := p.ProvisionServer(context.Background(), validRequest())
			if !errors.Is(err, errInjected) {
				t.Fatalf("err = %v, want the injected failure", err)
			}

			if n := len(ptero.users); n != 0 {
				t.Errorf("pterodactyl users left behind: %d", n)
			}
			if n := len(ptero.servers); n != 0 {
				t.Errorf("pterodactyl servers left behind: %d", n)
			}
			if n := len(store.customers); n != 0 {
				t.Errorf("customer rows left behind: %d", n)
			}
			if n := len(store.subscriptions); n != 0 {
				t.Errorf("subscription rows left behind: %d", n)
			}
			if n := len(store.servers); n != 0 {
				t.Errorf("server rows left behind: %d", n)
			}
			if len(dns.created) != 0 || len(queue.sent) != 0 {
				t.Errorf("side effects after failure: dns=%v emails=%d", dns.created, len(queue.sent))
			}
		})
	}
}
