package main

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/KentFarr/kuu-api/internal/pterodactyl"
)

// The fake is only useful if kuu-api's real client works against it, so
// this drives it through pterodactyl.Client end to end.
func TestFakePanelSpeaksClientProtocol(t *testing.T) {
	p := newPanel(1, "node1.test", 8192, 100000, []portRange{{25565, 25566}})
	var setupEmails []string
	p.onUserCreated = func(u pterodactyl.UserAttributes) { setupEmails = append(setupEmails, u.Email) }
	srv := httptest.NewServer(p.routes())
	defer srv.Close()

	ctx := context.Background()
	c := pterodactyl.NewClient(srv.URL, "test-key")

	if err := c.Ping(ctx); err != nil {
		t.Fatalf("Ping: %v", err)
	}

	user, source, err := c.CreateUser(ctx, "alice@example.com", "alice")
	if err != nil || user.Username != "alice" || source != pterodactyl.UsernameSourceUserChosen {
		t.Fatalf("CreateUser = %+v, %q, %v", user, source, err)
	}
	if len(setupEmails) != 1 {
		t.Errorf("setup emails = %v, want one for the passwordless user", setupEmails)
	}
	if found, err := c.FindUserByUsername(ctx, "alice"); err != nil || found == nil || found.ID != user.ID {
		t.Errorf("FindUserByUsername = %+v, %v", found, err)
	}

	nodeID, util, err := c.PickLeastLoadedNode(ctx, []int{1}, 2048)
	if err != nil || nodeID != 1 || util.FQDN != "node1.test" {
		t.Fatalf("PickLeastLoadedNode = %d, %+v, %v", nodeID, util, err)
	}
	alloc, err := c.FindFreeAllocation(ctx, 1, 25565, 25700)
	if err != nil || alloc.Port != 25565 {
		t.Fatalf("FindFreeAllocation = %+v, %v", alloc, err)
	}

	created, err := c.CreateServer(ctx, pterodactyl.CreateServerParams{Name: "mc", UserID: user.ID, AllocationID: alloc.ID, RAM: 2048})
	if err != nil {
		t.Fatalf("CreateServer: %v", err)
	}

	// Reusing a taken allocation is a 422, which kuu-api retries.
	_, err = c.CreateServer(ctx, pterodactyl.CreateServerParams{Name: "mc2", UserID: user.ID, AllocationID: alloc.ID, RAM: 2048})
	var apiErr *pterodactyl.APIError
	if !errors.As(err, &apiErr) || apiErr.StatusCode != http.StatusUnprocessableEntity {
		t.Errorf("reused allocation: err = %v, want a 422", err)
	}

	pterodactyl.InvalidateNodeCache(1)
	if u, err := c.GetNodeUtilization(ctx, 1, false); err != nil || u.MemoryUsedMB != 2048 || u.FreeAllocations != 1 {
		t.Errorf("utilization after create = %+v, %v; want 2048 MB used, 1 free allocation", u, err)
	}

	if err := c.SuspendServer(ctx, created.ID); err != nil {
		t.Fatalf("SuspendServer: %v", err)
	}
	servers, err := c.ListServersOnNode(ctx, 1)
	if err != nil || len(servers) != 1 || !servers[0].Suspended {
		t.Fatalf("ListServersOnNode = %+v, %v; want one suspended server", servers, err)
	}
	if err := c.UnsuspendServer(ctx, created.ID); err != nil {
		t.Fatalf("UnsuspendServer: %v", err)
	}
	if err := c.DeleteServer(ctx, created.ID); err != nil {
		t.Fatalf("DeleteServer: %v", err)
	}
	if err := c.DeleteUser(ctx, user.ID); err != nil {
		t.Fatalf("DeleteUser: %v", err)
	}
}

func TestFakePanelRequiresAPIKey(t *testing.T) {
	srv := httptest.NewServer(newPanel(1, "n", 1, 1, nil).routes())
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/api/application/nodes")
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401", resp.StatusCode)
	}
}
