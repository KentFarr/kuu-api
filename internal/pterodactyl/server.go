package pterodactyl

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
)

type ServerAttributes struct {
	ID         int    `json:"id"`
	Name       string `json:"name"`
	Identifier string `json:"identifier"`
}

type ServerResponse struct {
	Attributes ServerAttributes `json:"attributes"`
}

type CreateServerParams struct {
	Name         string
	UserID       int
	NestID       int
	EggID        int
	DockerImage  string
	Startup      string
	Environment  map[string]string
	AllocationID int
	RAM          int
	CPU          int
	Disk         int
}

func (c *Client) CreateServer(ctx context.Context, params CreateServerParams) (*ServerAttributes, error) {
	env := params.Environment
	if env == nil {
		env = map[string]string{}
	}

	body := map[string]interface{}{
		"name":         params.Name,
		"user":         params.UserID,
		"nest":         params.NestID,
		"egg":          params.EggID,
		"docker_image": params.DockerImage,
		"startup":      params.Startup,
		"environment":  env,
		"limits": map[string]int{
			"memory": params.RAM,
			"swap":   0,
			"disk":   params.Disk,
			"io":     500,
			"cpu":    params.CPU,
		},
		"feature_limits": map[string]int{
			"databases":   1,
			"allocations": 1,
			"backups":     3,
		},
		"allocation": map[string]int{
			"default": params.AllocationID,
		},
	}

	respBytes, err := c.doRequest(ctx, "POST", "/api/application/servers", body)
	if err != nil {
		return nil, fmt.Errorf("failed to create server: %w", err)
	}

	var resp ServerResponse
	if err := json.Unmarshal(respBytes, &resp); err != nil {
		return nil, fmt.Errorf("failed to parse server response: %w", err)
	}

	return &resp.Attributes, nil
}

func (c *Client) SuspendServer(ctx context.Context, serverID int) error {
	path := fmt.Sprintf("/api/application/servers/%d/suspend", serverID)
	_, err := c.doRequest(ctx, "POST", path, nil)
	if err != nil {
		var apiErr *APIError
		if errors.As(err, &apiErr) && apiErr.StatusCode == 404 {
			return nil
		}
		return fmt.Errorf("failed to suspend server %d: %w", serverID, err)
	}
	return nil
}

type listServersResponse struct {
	Data []struct {
		Attributes struct {
			ID        int    `json:"id"`
			Node      int    `json:"node"`
			User      int    `json:"user"`
			Suspended bool   `json:"suspended"`
			Name      string `json:"name"`
		} `json:"attributes"`
	} `json:"data"`
	Meta struct {
		Pagination struct {
			TotalPages  int `json:"total_pages"`
			CurrentPage int `json:"current_page"`
		} `json:"pagination"`
	} `json:"meta"`
}

// ServerSummary is the trimmed view returned by ListServersOnNode — just
// enough for reconciliation diffs without dragging in the full server
// object on every paginated page.
type ServerSummary struct {
	ID        int
	UserID    int
	Name      string
	Suspended bool
}

// ListServersOnNode pages through every server Pterodactyl reports on the
// given node. The panel's per-page cap is 100; we page until exhaustion.
// Used by the reconciliation endpoint, so admin-only and explicitly not in
// any hot path.
func (c *Client) ListServersOnNode(ctx context.Context, nodeID int) ([]ServerSummary, error) {
	var out []ServerSummary
	page := 1
	for {
		path := fmt.Sprintf("/api/application/servers?per_page=100&page=%d", page)
		respBytes, err := c.doRequest(ctx, "GET", path, nil)
		if err != nil {
			return nil, fmt.Errorf("failed to list servers (page %d): %w", page, err)
		}
		var resp listServersResponse
		if err := json.Unmarshal(respBytes, &resp); err != nil {
			return nil, fmt.Errorf("failed to parse server list (page %d): %w", page, err)
		}
		for _, s := range resp.Data {
			if s.Attributes.Node != nodeID {
				continue
			}
			out = append(out, ServerSummary{
				ID:        s.Attributes.ID,
				UserID:    s.Attributes.User,
				Name:      s.Attributes.Name,
				Suspended: s.Attributes.Suspended,
			})
		}
		if resp.Meta.Pagination.CurrentPage >= resp.Meta.Pagination.TotalPages {
			break
		}
		page++
	}
	return out, nil
}

func (c *Client) DeleteServer(ctx context.Context, serverID int) error {
	path := fmt.Sprintf("/api/application/servers/%d", serverID)
	_, err := c.doRequest(ctx, "DELETE", path, nil)
	if err != nil {
		var apiErr *APIError
		if errors.As(err, &apiErr) && apiErr.StatusCode == 404 {
			return nil
		}
		return fmt.Errorf("failed to delete server %d: %w", serverID, err)
	}
	return nil
}
