package pterodactyl

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"sync"
	"time"
)

// utilizationCacheTTL bounds how stale a node-utilization snapshot can be
// before we refetch. Short enough that bursts of provisioning don't keep
// landing on the same already-full node; long enough to avoid hammering
// the panel under sustained webhook load.
const utilizationCacheTTL = 10 * time.Second

// utilizationFetchTimeout caps the per-node panel call so one slow node
// can't stall the whole pick.
const utilizationFetchTimeout = 5 * time.Second

// ErrNoCapacity signals every node was reachable but none had enough free
// memory for the requested plan. Distinct from a panel reachability error
// so alerting can split "panel down" from "we're full."
var ErrNoCapacity = errors.New("no node has capacity for this plan")

type NodeUtilization struct {
	NodeID          int
	MemoryUsedMB    int
	MemoryTotalMB   int
	DiskUsedMB      int
	DiskTotalMB     int
	FreeAllocations int
}

// MemoryFreeMB returns headroom on this node in MB. Negative if Pterodactyl
// reports allocation above total (which can happen with overallocate set,
// but we treat it as "no headroom" for our scheduling purposes).
func (u *NodeUtilization) MemoryFreeMB() int {
	free := u.MemoryTotalMB - u.MemoryUsedMB
	if free < 0 {
		return 0
	}
	return free
}

type cachedUtil struct {
	util    *NodeUtilization
	fetched time.Time
}

var (
	utilCacheMu sync.Mutex
	utilCache   = map[int]cachedUtil{}
)

type nodeAttributes struct {
	ID                 int  `json:"id"`
	Memory             int  `json:"memory"`
	Disk               int  `json:"disk"`
	AllocatedResources struct {
		Memory int `json:"memory"`
		Disk   int `json:"disk"`
	} `json:"allocated_resources"`
}

type nodeResponse struct {
	Attributes nodeAttributes `json:"attributes"`
}

// GetNodeUtilization returns memory/disk usage plus a count of unassigned
// allocations on the given node. Results are cached for utilizationCacheTTL
// to keep panel load bounded under bursts. Pass useCache=false to force a
// refresh (used by the metrics ticker so gauges aren't ten seconds stale).
func (c *Client) GetNodeUtilization(ctx context.Context, nodeID int, useCache bool) (*NodeUtilization, error) {
	if useCache {
		utilCacheMu.Lock()
		entry, ok := utilCache[nodeID]
		utilCacheMu.Unlock()
		if ok && time.Since(entry.fetched) < utilizationCacheTTL {
			return entry.util, nil
		}
	}

	fetchCtx, cancel := context.WithTimeout(ctx, utilizationFetchTimeout)
	defer cancel()

	nodePath := fmt.Sprintf("/api/application/nodes/%d", nodeID)
	nodeBytes, err := c.doRequest(fetchCtx, "GET", nodePath, nil)
	if err != nil {
		return nil, fmt.Errorf("get node %d: %w", nodeID, err)
	}
	var nr nodeResponse
	if err := json.Unmarshal(nodeBytes, &nr); err != nil {
		return nil, fmt.Errorf("parse node %d: %w", nodeID, err)
	}

	allocPath := fmt.Sprintf("/api/application/nodes/%d/allocations?per_page=100", nodeID)
	allocBytes, err := c.doRequest(fetchCtx, "GET", allocPath, nil)
	if err != nil {
		return nil, fmt.Errorf("get allocations on node %d: %w", nodeID, err)
	}
	var ar AllocationListResponse
	if err := json.Unmarshal(allocBytes, &ar); err != nil {
		return nil, fmt.Errorf("parse allocations on node %d: %w", nodeID, err)
	}
	free := 0
	for _, a := range ar.Data {
		if !a.Attributes.Assigned {
			free++
		}
	}

	util := &NodeUtilization{
		NodeID:          nodeID,
		MemoryUsedMB:    nr.Attributes.AllocatedResources.Memory,
		MemoryTotalMB:   nr.Attributes.Memory,
		DiskUsedMB:      nr.Attributes.AllocatedResources.Disk,
		DiskTotalMB:     nr.Attributes.Disk,
		FreeAllocations: free,
	}

	utilCacheMu.Lock()
	utilCache[nodeID] = cachedUtil{util: util, fetched: time.Now()}
	utilCacheMu.Unlock()

	return util, nil
}

// PickLeastLoadedNode queries each candidate node in parallel and returns
// the one with the most free memory that still has at least one free
// allocation and enough headroom for requiredMB. Returns ErrNoCapacity if
// no node fits. If every node lookup errors, returns the underlying error
// from one of them so the caller can release the webhook for retry.
func (c *Client) PickLeastLoadedNode(ctx context.Context, nodeIDs []int, requiredMB int) (int, *NodeUtilization, error) {
	if len(nodeIDs) == 0 {
		return 0, nil, fmt.Errorf("no node IDs configured")
	}
	if len(nodeIDs) == 1 {
		// Skip the parallel utilization fetch for single-node deployments
		// — keeps the legacy single-node path fast and avoids surfacing
		// utilization-fetch failures as provisioning failures when there's
		// no actual choice to make.
		util, err := c.GetNodeUtilization(ctx, nodeIDs[0], true)
		if err != nil {
			return nodeIDs[0], nil, nil
		}
		return nodeIDs[0], util, nil
	}

	type result struct {
		util *NodeUtilization
		err  error
	}
	results := make([]result, len(nodeIDs))
	var wg sync.WaitGroup
	wg.Add(len(nodeIDs))
	for i, id := range nodeIDs {
		go func(i, id int) {
			defer wg.Done()
			u, err := c.GetNodeUtilization(ctx, id, true)
			results[i] = result{util: u, err: err}
		}(i, id)
	}
	wg.Wait()

	type candidate struct {
		util *NodeUtilization
	}
	var candidates []candidate
	var lastErr error
	for _, r := range results {
		if r.err != nil {
			lastErr = r.err
			continue
		}
		if r.util.FreeAllocations == 0 {
			continue
		}
		if r.util.MemoryFreeMB() < requiredMB {
			continue
		}
		candidates = append(candidates, candidate{util: r.util})
	}
	if len(candidates) == 0 {
		if lastErr != nil && len(results) > 0 {
			// Distinguish "every node lookup failed" (panel issue) from
			// "all nodes full" (capacity issue). If at least one node
			// returned successfully but no candidates remain, it's
			// capacity-bound.
			succeeded := 0
			for _, r := range results {
				if r.err == nil {
					succeeded++
				}
			}
			if succeeded == 0 {
				return 0, nil, fmt.Errorf("all node utilization lookups failed: %w", lastErr)
			}
		}
		return 0, nil, ErrNoCapacity
	}

	// Sort by free memory desc, tiebreak on free allocations desc.
	sort.Slice(candidates, func(i, j int) bool {
		if candidates[i].util.MemoryFreeMB() != candidates[j].util.MemoryFreeMB() {
			return candidates[i].util.MemoryFreeMB() > candidates[j].util.MemoryFreeMB()
		}
		return candidates[i].util.FreeAllocations > candidates[j].util.FreeAllocations
	})
	chosen := candidates[0].util
	return chosen.NodeID, chosen, nil
}

// InvalidateNodeCache drops the cached utilization snapshot for one node.
// Called by the provisioner after a successful create so a subsequent
// provision won't see the pre-create snapshot and over-pack the node.
func InvalidateNodeCache(nodeID int) {
	utilCacheMu.Lock()
	delete(utilCache, nodeID)
	utilCacheMu.Unlock()
}
