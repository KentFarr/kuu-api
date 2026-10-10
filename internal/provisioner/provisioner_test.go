package provisioner

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/KentFarr/kuu-api/internal/database"
	"github.com/KentFarr/kuu-api/internal/pterodactyl"
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
	// The panel sends the set-password link; our stored email must hold
	// no credentials.
	if body := queue.sent[0].Body; !strings.Contains(body, "Setup Your Account") || strings.Contains(body, "Password:") {
		t.Errorf("new-account email should point to the panel's setup email and contain no password:\n%s", body)
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
		{
			name:   "pterodactyl user creation fails",
			inject: func(ptero *fakePtero, _ *fakeStore) { ptero.fail["CreateUser"] = true },
		},
		{
			name:   "customer insert fails",
			inject: func(_ *fakePtero, store *fakeStore) { store.fail["CreateCustomer"] = true },
		},
		{
			name:   "subscription insert fails",
			inject: func(_ *fakePtero, store *fakeStore) { store.fail["CreateSubscription"] = true },
		},
		{
			name:   "no node has capacity",
			inject: func(ptero *fakePtero, _ *fakeStore) { ptero.fail["PickLeastLoadedNode"] = true },
		},
		{
			name:   "no free allocation",
			inject: func(ptero *fakePtero, _ *fakeStore) { ptero.fail["FindFreeAllocation"] = true },
		},
		{
			// A plain error isn't retried; a 422 would be.
			name:   "pterodactyl server creation fails",
			inject: func(ptero *fakePtero, _ *fakeStore) { ptero.fail["CreateServer"] = true },
		},
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

// returningCustomer stores a customer who already has a Pterodactyl account.
func returningCustomer(t *testing.T, store *fakeStore) *database.Customer {
	t.Helper()
	c, err := store.CreateCustomer("alice@example.com", 7, "alice")
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func TestProvisionServerReusesExistingCustomer(t *testing.T) {
	p, ptero, store, _, queue := newTestProvisioner(t)
	c := returningCustomer(t, store)

	res, err := p.ProvisionServer(context.Background(), validRequest())
	if err != nil {
		t.Fatalf("ProvisionServer: %v", err)
	}

	if len(ptero.users) != 0 {
		t.Errorf("created %d pterodactyl users for a returning customer", len(ptero.users))
	}
	if res.NewUser || res.UserID != 7 || res.Username != "alice" {
		t.Errorf("result = %+v, want existing user 7/alice", res)
	}
	if len(store.customers) != 1 {
		t.Errorf("customers = %d, want the existing one only", len(store.customers))
	}
	for _, s := range store.servers {
		if s.CustomerID != c.ID {
			t.Errorf("server row customer = %d, want %d", s.CustomerID, c.ID)
		}
	}
	// Returning customers get a server-ready email too, telling them to
	// use their existing login rather than waiting for a setup email.
	if len(queue.sent) != 1 || !strings.Contains(queue.sent[0].Body, "existing Kuu panel account") {
		t.Errorf("emails = %+v, want one server-ready email for the existing account", queue.sent)
	}
}

// Rollback must only undo what this call created: a returning customer's
// row and panel account were there before and must survive a failure.
func TestProvisionServerRollbackKeepsPreexistingCustomer(t *testing.T) {
	p, _, store, _, _ := newTestProvisioner(t)
	c := returningCustomer(t, store)
	store.fail["CreateServer"] = true

	if _, err := p.ProvisionServer(context.Background(), validRequest()); !errors.Is(err, errInjected) {
		t.Fatalf("err = %v, want the injected failure", err)
	}

	if _, ok := store.customers[c.ID]; !ok {
		t.Error("pre-existing customer row was deleted by rollback")
	}
	if len(store.subscriptions) != 0 {
		t.Errorf("subscription rows left behind: %d", len(store.subscriptions))
	}
}

func TestProvisionServerDNSFailureKeepsServer(t *testing.T) {
	p, ptero, store, dns, queue := newTestProvisioner(t)
	dns.fail["CreateCNAME"] = true

	if _, err := p.ProvisionServer(context.Background(), validRequest()); err != nil {
		t.Fatalf("ProvisionServer: %v (DNS failure must not fail provisioning)", err)
	}
	if len(ptero.servers) != 1 || len(store.servers) != 1 {
		t.Errorf("servers: panel=%d db=%d, want 1 each", len(ptero.servers), len(store.servers))
	}
	if len(queue.sent) != 1 {
		t.Errorf("welcome emails = %d, want 1", len(queue.sent))
	}
}

func TestProvisionServerWithoutOptionalDeps(t *testing.T) {
	p, _, _, _, _ := newTestProvisioner(t)
	p.DNSClient = nil
	p.EmailQueue = nil

	if _, err := p.ProvisionServer(context.Background(), validRequest()); err != nil {
		t.Fatalf("ProvisionServer without DNS/email: %v", err)
	}
}

func TestProvisionServerRetriesAllocationCollision(t *testing.T) {
	p, ptero, store, _, _ := newTestProvisioner(t)
	ptero.createServerErrs = []error{&pterodactyl.APIError{StatusCode: 422, Body: "allocation taken"}}

	if _, err := p.ProvisionServer(context.Background(), validRequest()); err != nil {
		t.Fatalf("ProvisionServer: %v", err)
	}
	if ptero.createServerCalls != 2 {
		t.Errorf("CreateServer calls = %d, want 2 (one 422, one success)", ptero.createServerCalls)
	}
	if len(store.servers) != 1 {
		t.Errorf("server rows = %d, want 1", len(store.servers))
	}
}

func TestProvisionServerRejectsInvalidRequests(t *testing.T) {
	cases := map[string]func(*Request){
		"unknown price ID": func(r *Request) { r.PriceID = "price_nope" },
		"unknown game":     func(r *Request) { r.Game = "tetris" },
		"unknown variant":  func(r *Request) { r.Variant = "bukkit" },
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			p, ptero, store, _, _ := newTestProvisioner(t)
			req := validRequest()
			mutate(&req)

			if _, err := p.ProvisionServer(context.Background(), req); err == nil {
				t.Fatal("expected an error")
			}
			if len(ptero.users)+len(ptero.servers)+len(store.customers)+len(store.subscriptions) != 0 {
				t.Error("invalid request created resources")
			}
		})
	}
}

func TestSuspendServer(t *testing.T) {
	p, ptero, store, _, _ := newTestProvisioner(t)
	res, err := p.ProvisionServer(context.Background(), validRequest())
	if err != nil {
		t.Fatal(err)
	}

	if err := p.SuspendServer(context.Background(), "sub_123", database.SuspendReasonCancelled); err != nil {
		t.Fatalf("SuspendServer: %v", err)
	}

	if !ptero.suspended[res.ServerID] {
		t.Error("pterodactyl server not suspended")
	}
	for _, s := range store.subscriptions {
		if s.Status != "suspended" {
			t.Errorf("subscription status = %q, want suspended", s.Status)
		}
	}
	for _, s := range store.servers {
		if s.Status != "suspended" {
			t.Errorf("server status = %q, want suspended", s.Status)
		}
	}
}

func TestSuspendServerErrors(t *testing.T) {
	t.Run("unknown subscription", func(t *testing.T) {
		p, _, _, _, _ := newTestProvisioner(t)
		if err := p.SuspendServer(context.Background(), "sub_missing", database.SuspendReasonCancelled); err == nil {
			t.Error("expected an error")
		}
	})
	t.Run("panel suspend fails", func(t *testing.T) {
		p, ptero, store, _, _ := newTestProvisioner(t)
		if _, err := p.ProvisionServer(context.Background(), validRequest()); err != nil {
			t.Fatal(err)
		}
		ptero.fail["SuspendServer"] = true

		if err := p.SuspendServer(context.Background(), "sub_123", database.SuspendReasonNonPayment); !errors.Is(err, errInjected) {
			t.Fatalf("err = %v, want the injected failure", err)
		}
		// The DB must not claim the server is suspended when it isn't.
		for _, s := range store.subscriptions {
			if s.Status != "active" {
				t.Errorf("subscription status = %q, want active", s.Status)
			}
		}
	})
}
