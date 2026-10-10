package provisioner

import (
	"context"
	"testing"
	"time"

	"github.com/KentFarr/kuu-api/internal/database"
	"github.com/KentFarr/kuu-api/internal/pterodactyl"
)

// addSuspendedServer stores a suspended subscription cancelled at
// cancelledAt, with a suspended server on it, and returns the server row ID.
func addSuspendedServer(store *fakeStore, ptero *fakePtero, cancelledAt time.Time) int {
	sub, _ := store.CreateSubscription(1, "sub_"+cancelledAt.Format("20060102"), testPriceID, "minecraft")
	store.subscriptions[sub.ID].Status = "suspended"
	store.subscriptions[sub.ID].CancelledAt = &cancelledAt

	ptero.nextID++
	ptero.servers[ptero.nextID] = pterodactyl.CreateServerParams{Name: "srv"}
	srv, _ := store.CreateServer(database.CreateServerParams{
		SubscriptionID:      sub.ID,
		PterodactylServerID: ptero.nextID,
		ServerName:          "srv",
	})
	store.servers[srv.ID].Status = "suspended"
	return srv.ID
}

func TestReaperDeletesOnlyServersPastGracePeriod(t *testing.T) {
	p, ptero, store, _, _ := newTestProvisioner(t)
	now := time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC)

	r := NewReaper(p)
	r.Now = func() time.Time { return now }

	oldID := addSuspendedServer(store, ptero, now.Add(-15*24*time.Hour))
	recentID := addSuspendedServer(store, ptero, now.Add(-13*24*time.Hour))

	r.runOnce(context.Background())

	if want := now.Add(-14 * 24 * time.Hour); !store.reapCutoff.Equal(want) {
		t.Errorf("cutoff = %s, want %s", store.reapCutoff, want)
	}
	if _, ok := store.servers[oldID]; ok {
		t.Error("server suspended 15 days ago was not reaped")
	}
	if _, ok := store.servers[recentID]; !ok {
		t.Error("server suspended 13 days ago was reaped")
	}
	if n := len(ptero.servers); n != 1 {
		t.Errorf("pterodactyl servers remaining = %d, want 1", n)
	}
}
