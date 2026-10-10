package database

import (
	"testing"
	"time"
)

// mustExec runs setup SQL that the public API has no method for, such as
// back-dating a timestamp.
func mustExec(t *testing.T, db *DB, query string, args ...any) {
	t.Helper()
	if _, err := db.conn.Exec(query, args...); err != nil {
		t.Fatalf("exec %q: %v", query, err)
	}
}

func TestCustomerLifecycle(t *testing.T) {
	db := newTestDB(t)

	c, err := db.CreateCustomer("alice@example.com", 7, "alice")
	if err != nil {
		t.Fatalf("CreateCustomer: %v", err)
	}
	if c.ID == 0 || *c.PterodactylUserID != 7 || *c.PterodactylUsername != "alice" || c.StripeCustomerID != nil {
		t.Errorf("created customer = %+v", c)
	}

	if _, err := db.CreateCustomer("alice@example.com", 8, "alice2"); err == nil {
		t.Error("duplicate email accepted")
	}

	if err := db.UpdateStripeCustomerID(c.ID, "cus_1"); err != nil {
		t.Fatalf("UpdateStripeCustomerID: %v", err)
	}
	got, err := db.GetCustomerByEmail("alice@example.com")
	if err != nil || got == nil || got.StripeCustomerID == nil || *got.StripeCustomerID != "cus_1" {
		t.Fatalf("GetCustomerByEmail = %+v, %v; want stripe ID cus_1", got, err)
	}

	missing, err := db.GetCustomerByEmail("nobody@example.com")
	if missing != nil || err != nil {
		t.Errorf("missing customer = %+v, %v; want nil, nil", missing, err)
	}

	if err := db.DeleteCustomer(c.ID); err != nil {
		t.Fatalf("DeleteCustomer: %v", err)
	}
	if got, _ := db.GetCustomerByEmail("alice@example.com"); got != nil {
		t.Error("customer still present after delete")
	}
}

// seedServer creates a customer, an active subscription and an active
// server, returning all three.
func seedServer(t *testing.T, db *DB, email, stripeSubID string, pteroServerID int) (*Customer, *Subscription, *Server) {
	t.Helper()
	c, err := db.CreateCustomer(email, pteroServerID, "user")
	if err != nil {
		t.Fatalf("CreateCustomer: %v", err)
	}
	sub, err := db.CreateSubscription(c.ID, stripeSubID, "price_2gb", "minecraft")
	if err != nil {
		t.Fatalf("CreateSubscription: %v", err)
	}
	srv, err := db.CreateServer(CreateServerParams{
		SubscriptionID:      sub.ID,
		CustomerID:          c.ID,
		PterodactylServerID: pteroServerID,
		NodeID:              1,
		Port:                25565,
		ServerName:          "srv-" + stripeSubID,
		Game:                "minecraft",
		Variant:             "paper",
		PlanName:            "2GB",
	})
	if err != nil {
		t.Fatalf("CreateServer: %v", err)
	}
	return c, sub, srv
}

func TestSubscriptionAndServerLifecycle(t *testing.T) {
	db := newTestDB(t)
	c, sub, srv := seedServer(t, db, "alice@example.com", "sub_1", 100)

	if sub.Status != "active" || sub.CancelledAt != nil {
		t.Errorf("new subscription = %+v, want active and not cancelled", sub)
	}
	// Empty optional strings are stored as NULL, set ones as values.
	if !srv.Variant.Valid || srv.Variant.String != "paper" || srv.Version.Valid || srv.Subdomain.Valid {
		t.Errorf("server optional columns: variant=%v version=%v subdomain=%v", srv.Variant, srv.Version, srv.Subdomain)
	}

	got, err := db.GetSubscriptionByStripeID("sub_1")
	if err != nil || got.ID != sub.ID {
		t.Fatalf("GetSubscriptionByStripeID = %+v, %v", got, err)
	}
	if _, err := db.GetSubscriptionByStripeID("sub_missing"); err == nil {
		t.Error("missing subscription: expected an error")
	}

	bySub, err := db.GetServerBySubscription(sub.ID)
	if err != nil || bySub.ID != srv.ID {
		t.Fatalf("GetServerBySubscription = %+v, %v", bySub, err)
	}
	latest, err := db.GetLatestActiveServerByCustomerID(c.ID)
	if err != nil || latest == nil || latest.ID != srv.ID {
		t.Fatalf("GetLatestActiveServerByCustomerID = %+v, %v", latest, err)
	}
	ids, err := db.ListServerPteroIDs(1)
	if err != nil || len(ids) != 1 || ids[0] != 100 {
		t.Errorf("ListServerPteroIDs(1) = %v, %v; want [100]", ids, err)
	}

	if err := db.SuspendSubscription("sub_1"); err != nil {
		t.Fatalf("SuspendSubscription: %v", err)
	}
	if err := db.SuspendServer(sub.ID); err != nil {
		t.Fatalf("SuspendServer: %v", err)
	}
	got, _ = db.GetSubscriptionByStripeID("sub_1")
	if got.Status != "suspended" || got.CancelledAt == nil {
		t.Errorf("suspended subscription = %+v, want status suspended with cancelled_at", got)
	}
	if latest, _ := db.GetLatestActiveServerByCustomerID(c.ID); latest != nil {
		t.Error("suspended server still returned as the latest active one")
	}

	// Foreign keys are ON DELETE RESTRICT: children go first.
	if err := db.DeleteSubscription(sub.ID); err == nil {
		t.Error("deleted a subscription that still has a server")
	}
	if err := db.DeleteServerByID(srv.ID); err != nil {
		t.Fatalf("DeleteServerByID: %v", err)
	}
	if err := db.DeleteSubscription(sub.ID); err != nil {
		t.Fatalf("DeleteSubscription: %v", err)
	}
}

func TestListReapableServers(t *testing.T) {
	db := newTestDB(t)

	_, oldSub, oldSrv := seedServer(t, db, "old@example.com", "sub_old", 1)
	_, newSub, _ := seedServer(t, db, "new@example.com", "sub_new", 2)
	seedServer(t, db, "active@example.com", "sub_active", 3)

	for _, sub := range []*Subscription{oldSub, newSub} {
		if err := db.SuspendSubscription(sub.StripeSubscriptionID); err != nil {
			t.Fatal(err)
		}
		if err := db.SuspendServer(sub.ID); err != nil {
			t.Fatal(err)
		}
	}
	mustExec(t, db, `UPDATE subscriptions SET cancelled_at = NOW() - INTERVAL '15 days' WHERE id = $1`, oldSub.ID)
	mustExec(t, db, `UPDATE subscriptions SET cancelled_at = NOW() - INTERVAL '13 days' WHERE id = $1`, newSub.ID)

	cutoff := time.Now().UTC().Add(-14 * 24 * time.Hour)
	servers, err := db.ListReapableServers(cutoff)
	if err != nil {
		t.Fatalf("ListReapableServers: %v", err)
	}
	if len(servers) != 1 || servers[0].ID != oldSrv.ID {
		t.Errorf("reapable = %+v, want only the server suspended 15 days ago", servers)
	}
}
