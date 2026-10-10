package provisioner

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/KentFarr/kuu-api/internal/database"
	"github.com/KentFarr/kuu-api/internal/pterodactyl"
)

// errInjected is what a fake returns for a method listed in its failOn map.
var errInjected = errors.New("injected failure")

// testPriceID is wired to the 2GB plan by newTestProvisioner.
const testPriceID = "price_test_2gb"

// newTestProvisioner returns a Provisioner backed by empty fakes, plus the
// fakes so the test can inspect them or inject failures.
func newTestProvisioner(t *testing.T) (*Provisioner, *fakePtero, *fakeStore, *fakeDNS, *fakeQueue) {
	t.Helper()
	// plans reads price IDs from the environment on every lookup.
	t.Setenv("STRIPE_PRICE_2GB", testPriceID)

	// Keep allocation retries from sleeping for seconds.
	origBackoff := allocationRetryBackoff
	allocationRetryBackoff = []time.Duration{time.Millisecond}
	t.Cleanup(func() { allocationRetryBackoff = origBackoff })

	ptero := newFakePtero()
	store := newFakeStore()
	dns := &fakeDNS{fail: failOn{}}
	queue := &fakeQueue{fail: failOn{}}
	p := &Provisioner{
		PteroClient: ptero,
		DB:          store,
		EmailQueue:  queue,
		DNSClient:   dns,
		NodeIDs:     []int{1},
	}
	return p, ptero, store, dns, queue
}

// failOn makes the named method on a fake return errInjected.
type failOn map[string]bool

func (f failOn) check(method string) error {
	if f[method] {
		return fmt.Errorf("%s: %w", method, errInjected)
	}
	return nil
}

// ---------------------------------------------------------------------------
// Pterodactyl

type fakePtero struct {
	mu        sync.Mutex
	fail      failOn
	nextID    int
	users     map[int]pterodactyl.UserAttributes
	servers   map[int]pterodactyl.CreateServerParams
	suspended map[int]bool

	// createServerErrs are returned by successive CreateServer calls before
	// it starts succeeding, e.g. a 422 to simulate an allocation collision.
	createServerErrs  []error
	createServerCalls int
}

func newFakePtero() *fakePtero {
	return &fakePtero{
		fail:      failOn{},
		nextID:    100,
		users:     map[int]pterodactyl.UserAttributes{},
		servers:   map[int]pterodactyl.CreateServerParams{},
		suspended: map[int]bool{},
	}
}

func (f *fakePtero) CreateUser(_ context.Context, email, requestedUsername string) (*pterodactyl.UserAttributes, pterodactyl.UsernameSource, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.fail.check("CreateUser"); err != nil {
		return nil, "", err
	}
	f.nextID++
	username := requestedUsername
	if username == "" {
		username = fmt.Sprintf("kuu_user%d", f.nextID)
	}
	u := pterodactyl.UserAttributes{ID: f.nextID, Username: username, Email: email}
	f.users[u.ID] = u
	return &u, pterodactyl.UsernameSource("test"), nil
}

func (f *fakePtero) DeleteUser(_ context.Context, userID int) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.fail.check("DeleteUser"); err != nil {
		return err
	}
	delete(f.users, userID)
	return nil
}

func (f *fakePtero) PickLeastLoadedNode(_ context.Context, nodeIDs []int, _ int) (int, *pterodactyl.NodeUtilization, error) {
	if err := f.fail.check("PickLeastLoadedNode"); err != nil {
		return 0, nil, err
	}
	return nodeIDs[0], &pterodactyl.NodeUtilization{NodeID: nodeIDs[0], FQDN: "node1.test"}, nil
}

func (f *fakePtero) FindFreeAllocation(_ context.Context, _ int, minPort, _ int) (*pterodactyl.AllocationAttributes, error) {
	if err := f.fail.check("FindFreeAllocation"); err != nil {
		return nil, err
	}
	return &pterodactyl.AllocationAttributes{ID: 500, Port: minPort}, nil
}

func (f *fakePtero) CreateServer(_ context.Context, params pterodactyl.CreateServerParams) (*pterodactyl.ServerAttributes, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.createServerCalls++
	if err := f.fail.check("CreateServer"); err != nil {
		return nil, err
	}
	if len(f.createServerErrs) > 0 {
		err := f.createServerErrs[0]
		f.createServerErrs = f.createServerErrs[1:]
		return nil, err
	}
	f.nextID++
	f.servers[f.nextID] = params
	return &pterodactyl.ServerAttributes{ID: f.nextID, Name: params.Name}, nil
}

func (f *fakePtero) SuspendServer(_ context.Context, serverID int) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.fail.check("SuspendServer"); err != nil {
		return err
	}
	f.suspended[serverID] = true
	return nil
}

func (f *fakePtero) DeleteServer(_ context.Context, serverID int) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.fail.check("DeleteServer"); err != nil {
		return err
	}
	delete(f.servers, serverID)
	return nil
}

// ---------------------------------------------------------------------------
// Database

// fakeStore keeps rows in maps keyed by ID. It doesn't enforce foreign keys
// or unique constraints; database integration tests cover those.
type fakeStore struct {
	mu            sync.Mutex
	fail          failOn
	nextID        int
	customers     map[int]*database.Customer
	subscriptions map[int]*database.Subscription
	servers       map[int]*database.Server

	// reapCutoff records the cutoff passed to ListReapableServers.
	reapCutoff time.Time
}

func newFakeStore() *fakeStore {
	return &fakeStore{
		fail:          failOn{},
		customers:     map[int]*database.Customer{},
		subscriptions: map[int]*database.Subscription{},
		servers:       map[int]*database.Server{},
	}
}

func (f *fakeStore) id() int { f.nextID++; return f.nextID }

func (f *fakeStore) GetCustomerByEmail(email string) (*database.Customer, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.fail.check("GetCustomerByEmail"); err != nil {
		return nil, err
	}
	for _, c := range f.customers {
		if c.Email == email {
			cp := *c
			return &cp, nil
		}
	}
	return nil, nil // matches *database.DB: no row is not an error
}

func (f *fakeStore) CreateCustomer(email string, pteroUserID int, pteroUsername string) (*database.Customer, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.fail.check("CreateCustomer"); err != nil {
		return nil, err
	}
	c := &database.Customer{ID: f.id(), Email: email, PterodactylUserID: &pteroUserID, PterodactylUsername: &pteroUsername}
	f.customers[c.ID] = c
	cp := *c
	return &cp, nil
}

func (f *fakeStore) UpdateStripeCustomerID(customerID int, stripeCustomerID string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.fail.check("UpdateStripeCustomerID"); err != nil {
		return err
	}
	if c, ok := f.customers[customerID]; ok {
		c.StripeCustomerID = &stripeCustomerID
	}
	return nil
}

func (f *fakeStore) DeleteCustomer(customerID int) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.fail.check("DeleteCustomer"); err != nil {
		return err
	}
	delete(f.customers, customerID)
	return nil
}

func (f *fakeStore) CreateSubscription(customerID int, stripeSubID, stripePriceID, game string) (*database.Subscription, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.fail.check("CreateSubscription"); err != nil {
		return nil, err
	}
	s := &database.Subscription{ID: f.id(), CustomerID: customerID, StripeSubscriptionID: stripeSubID, StripePriceID: stripePriceID, Game: game, Status: "active"}
	f.subscriptions[s.ID] = s
	cp := *s
	return &cp, nil
}

func (f *fakeStore) GetSubscriptionByStripeID(stripeSubID string) (*database.Subscription, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.fail.check("GetSubscriptionByStripeID"); err != nil {
		return nil, err
	}
	for _, s := range f.subscriptions {
		if s.StripeSubscriptionID == stripeSubID {
			cp := *s
			return &cp, nil
		}
	}
	return nil, errors.New("subscription not found") // matches *database.DB
}

func (f *fakeStore) SuspendSubscription(stripeSubID string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.fail.check("SuspendSubscription"); err != nil {
		return err
	}
	for _, s := range f.subscriptions {
		if s.StripeSubscriptionID == stripeSubID {
			s.Status = "suspended"
		}
	}
	return nil
}

func (f *fakeStore) DeleteSubscription(subscriptionID int) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.fail.check("DeleteSubscription"); err != nil {
		return err
	}
	delete(f.subscriptions, subscriptionID)
	return nil
}

func (f *fakeStore) CreateServer(p database.CreateServerParams) (*database.Server, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.fail.check("CreateServer"); err != nil {
		return nil, err
	}
	s := &database.Server{
		ID:                  f.id(),
		SubscriptionID:      p.SubscriptionID,
		CustomerID:          p.CustomerID,
		PterodactylServerID: p.PterodactylServerID,
		ServerName:          p.ServerName,
		NodeID:              p.NodeID,
		Port:                p.Port,
		Game:                p.Game,
		PlanName:            p.PlanName,
		Status:              "active",
	}
	s.Variant.String, s.Variant.Valid = p.Variant, p.Variant != ""
	s.Version.String, s.Version.Valid = p.Version, p.Version != ""
	s.Subdomain.String, s.Subdomain.Valid = p.Subdomain, p.Subdomain != ""
	f.servers[s.ID] = s
	cp := *s
	return &cp, nil
}

func (f *fakeStore) GetServerBySubscription(subscriptionID int) (*database.Server, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.fail.check("GetServerBySubscription"); err != nil {
		return nil, err
	}
	for _, s := range f.servers {
		if s.SubscriptionID == subscriptionID {
			cp := *s
			return &cp, nil
		}
	}
	return nil, errors.New("server not found") // matches *database.DB
}

func (f *fakeStore) SuspendServer(subscriptionID int) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.fail.check("SuspendServer"); err != nil {
		return err
	}
	for _, s := range f.servers {
		if s.SubscriptionID == subscriptionID {
			s.Status = "suspended"
		}
	}
	return nil
}

// ListReapableServers mirrors the SQL: suspended servers whose subscription
// was cancelled before cutoff.
func (f *fakeStore) ListReapableServers(cutoff time.Time) ([]database.Server, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.reapCutoff = cutoff
	if err := f.fail.check("ListReapableServers"); err != nil {
		return nil, err
	}
	var out []database.Server
	for _, s := range f.servers {
		sub, ok := f.subscriptions[s.SubscriptionID]
		if !ok || s.Status != "suspended" || sub.Status != "suspended" || sub.CancelledAt == nil {
			continue
		}
		if sub.CancelledAt.Before(cutoff) {
			out = append(out, *s)
		}
	}
	return out, nil
}

func (f *fakeStore) DeleteServerByID(id int) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.fail.check("DeleteServerByID"); err != nil {
		return err
	}
	delete(f.servers, id)
	return nil
}

// ---------------------------------------------------------------------------
// DNS and email

type fakeDNS struct {
	mu      sync.Mutex
	fail    failOn
	created map[string]string // subdomain -> target
	deleted []string
}

func (f *fakeDNS) CreateCNAME(_ context.Context, subdomain, target string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.fail.check("CreateCNAME"); err != nil {
		return err
	}
	if f.created == nil {
		f.created = map[string]string{}
	}
	f.created[subdomain] = target
	return nil
}

func (f *fakeDNS) DeleteCNAME(_ context.Context, subdomain string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.fail.check("DeleteCNAME"); err != nil {
		return err
	}
	f.deleted = append(f.deleted, subdomain)
	return nil
}

type sentEmail struct{ To, Subject, Body string }

type fakeQueue struct {
	mu   sync.Mutex
	fail failOn
	sent []sentEmail
}

func (f *fakeQueue) Enqueue(toAddr, subject, body string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.fail.check("Enqueue"); err != nil {
		return err
	}
	f.sent = append(f.sent, sentEmail{To: toAddr, Subject: subject, Body: body})
	return nil
}
