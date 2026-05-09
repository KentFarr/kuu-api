package pterodactyl

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// nodeFixture lets each test case describe per-node fake responses without
// rebuilding the same httptest scaffolding every time.
type nodeFixture struct {
	memoryTotal     int
	memoryUsed      int
	freeAllocations int
	failNode        bool
	failAllocations bool
}

func newPickServer(t *testing.T, fixtures map[int]nodeFixture) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	for id, f := range fixtures {
		id, f := id, f // capture
		mux.HandleFunc(fmt.Sprintf("/api/application/nodes/%d", id), func(w http.ResponseWriter, r *http.Request) {
			// Distinguish exact node path from /allocations subpath.
			if strings.HasSuffix(r.URL.Path, fmt.Sprintf("/nodes/%d", id)) {
				if f.failNode {
					http.Error(w, "boom", http.StatusInternalServerError)
					return
				}
				w.Header().Set("Content-Type", "application/json")
				fmt.Fprintf(w, `{"attributes":{"id":%d,"memory":%d,"disk":100000,"allocated_resources":{"memory":%d,"disk":0}}}`,
					id, f.memoryTotal, f.memoryUsed)
			}
		})
		mux.HandleFunc(fmt.Sprintf("/api/application/nodes/%d/allocations", id), func(w http.ResponseWriter, r *http.Request) {
			if f.failAllocations {
				http.Error(w, "boom", http.StatusInternalServerError)
				return
			}
			w.Header().Set("Content-Type", "application/json")
			var data strings.Builder
			data.WriteString(`{"data":[`)
			for i := 0; i < f.freeAllocations; i++ {
				if i > 0 {
					data.WriteString(",")
				}
				fmt.Fprintf(&data, `{"attributes":{"id":%d,"port":%d,"assigned":false}}`, 1000+i, 25565+i)
			}
			data.WriteString(`]}`)
			fmt.Fprint(w, data.String())
		})
	}
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

func resetUtilCache() {
	utilCacheMu.Lock()
	utilCache = map[int]cachedUtil{}
	utilCacheMu.Unlock()
}

func TestPickLeastLoadedNodeChoosesMostFreeMemory(t *testing.T) {
	resetUtilCache()
	srv := newPickServer(t, map[int]nodeFixture{
		1: {memoryTotal: 16384, memoryUsed: 8000, freeAllocations: 3},
		2: {memoryTotal: 16384, memoryUsed: 2000, freeAllocations: 3}, // most free
		3: {memoryTotal: 16384, memoryUsed: 14000, freeAllocations: 3},
	})
	c := &Client{BaseURL: srv.URL, APIKey: "test", HTTPClient: srv.Client()}

	id, util, err := c.PickLeastLoadedNode(context.Background(), []int{1, 2, 3}, 1024)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if id != 2 {
		t.Fatalf("expected node 2 (most free memory), got %d", id)
	}
	if util == nil || util.NodeID != 2 {
		t.Fatalf("expected utilization for node 2, got %+v", util)
	}
}

func TestPickLeastLoadedNodeFiltersInsufficientMemory(t *testing.T) {
	resetUtilCache()
	srv := newPickServer(t, map[int]nodeFixture{
		1: {memoryTotal: 16384, memoryUsed: 16000, freeAllocations: 3}, // only 384 free
		2: {memoryTotal: 16384, memoryUsed: 12000, freeAllocations: 3}, // 4384 free
	})
	c := &Client{BaseURL: srv.URL, APIKey: "test", HTTPClient: srv.Client()}

	// Plan needs 2GB; only node 2 fits.
	id, _, err := c.PickLeastLoadedNode(context.Background(), []int{1, 2}, 2048)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if id != 2 {
		t.Fatalf("expected node 2 (sufficient headroom), got %d", id)
	}
}

func TestPickLeastLoadedNodeNoCapacity(t *testing.T) {
	resetUtilCache()
	srv := newPickServer(t, map[int]nodeFixture{
		1: {memoryTotal: 16384, memoryUsed: 16000, freeAllocations: 3},
		2: {memoryTotal: 16384, memoryUsed: 16384, freeAllocations: 3},
	})
	c := &Client{BaseURL: srv.URL, APIKey: "test", HTTPClient: srv.Client()}

	_, _, err := c.PickLeastLoadedNode(context.Background(), []int{1, 2}, 4096)
	if !errors.Is(err, ErrNoCapacity) {
		t.Fatalf("expected ErrNoCapacity, got %v", err)
	}
}

func TestPickLeastLoadedNodeFiltersZeroFreeAllocations(t *testing.T) {
	resetUtilCache()
	srv := newPickServer(t, map[int]nodeFixture{
		1: {memoryTotal: 16384, memoryUsed: 1000, freeAllocations: 0}, // no ports
		2: {memoryTotal: 16384, memoryUsed: 8000, freeAllocations: 1}, // less free mem but has ports
	})
	c := &Client{BaseURL: srv.URL, APIKey: "test", HTTPClient: srv.Client()}

	id, _, err := c.PickLeastLoadedNode(context.Background(), []int{1, 2}, 1024)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if id != 2 {
		t.Fatalf("expected node 2 (only one with free allocations), got %d", id)
	}
}

func TestPickLeastLoadedNodeAllNodesUnreachable(t *testing.T) {
	resetUtilCache()
	srv := newPickServer(t, map[int]nodeFixture{
		1: {failNode: true},
		2: {failNode: true},
	})
	c := &Client{BaseURL: srv.URL, APIKey: "test", HTTPClient: srv.Client()}

	_, _, err := c.PickLeastLoadedNode(context.Background(), []int{1, 2}, 1024)
	if err == nil {
		t.Fatal("expected error when every lookup fails")
	}
	if errors.Is(err, ErrNoCapacity) {
		t.Fatalf("expected lookup error, not ErrNoCapacity: %v", err)
	}
}

func TestPickLeastLoadedNodeSingleNodePassthrough(t *testing.T) {
	resetUtilCache()
	srv := newPickServer(t, map[int]nodeFixture{
		1: {memoryTotal: 16384, memoryUsed: 1000, freeAllocations: 5},
	})
	c := &Client{BaseURL: srv.URL, APIKey: "test", HTTPClient: srv.Client()}

	id, _, err := c.PickLeastLoadedNode(context.Background(), []int{1}, 8192)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if id != 1 {
		t.Fatalf("expected single configured node 1, got %d", id)
	}
}

func TestPickLeastLoadedNodeTiebreaksOnFreeAllocations(t *testing.T) {
	resetUtilCache()
	srv := newPickServer(t, map[int]nodeFixture{
		1: {memoryTotal: 16384, memoryUsed: 4000, freeAllocations: 2},
		2: {memoryTotal: 16384, memoryUsed: 4000, freeAllocations: 8}, // tie on memory, more ports
	})
	c := &Client{BaseURL: srv.URL, APIKey: "test", HTTPClient: srv.Client()}

	id, _, err := c.PickLeastLoadedNode(context.Background(), []int{1, 2}, 1024)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if id != 2 {
		t.Fatalf("expected node 2 (tiebreak on free allocations), got %d", id)
	}
}
