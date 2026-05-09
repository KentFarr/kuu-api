package pterodactyl

import (
	"context"
	"encoding/json"
	"fmt"
)

type AllocationAttributes struct {
	ID       int  `json:"id"`
	Port     int  `json:"port"`
	Assigned bool `json:"assigned"`
}

type AllocationData struct {
	Attributes AllocationAttributes `json:"attributes"`
}

type AllocationListResponse struct {
	Data []AllocationData `json:"data"`
}

func (c *Client) FindFreeAllocation(ctx context.Context, nodeID int, minPort, maxPort int) (*AllocationAttributes, error) {
	path := fmt.Sprintf("/api/application/nodes/%d/allocations", nodeID)

	respBytes, err := c.doRequest(ctx, "GET", path, nil)
	if err != nil {
		return nil, fmt.Errorf("failed to list allocations: %w", err)
	}

	var resp AllocationListResponse
	if err := json.Unmarshal(respBytes, &resp); err != nil {
		return nil, fmt.Errorf("failed to parse allocations: %w", err)
	}

	for _, alloc := range resp.Data {
		if !alloc.Attributes.Assigned {
			// If range is provided, check it.
			if minPort > 0 && (alloc.Attributes.Port < minPort || alloc.Attributes.Port > maxPort) {
				continue
			}
			return &alloc.Attributes, nil
		}
	}

	if minPort > 0 {
		return nil, fmt.Errorf("no free allocations on node %d in range %d-%d", nodeID, minPort, maxPort)
	}
	return nil, fmt.Errorf("no free allocations on node %d", nodeID)
}
