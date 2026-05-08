package pterodactyl

import (
	"encoding/json"
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
	Name          string
	UserID        int
	NestID        int
	EggID         int
	DockerImage   string
	AllocationID  int
	RAM           int
	CPU           int
	Disk          int
}

func (c *Client) CreateServer(params CreateServerParams) (*ServerAttributes, error) {
	body := map[string]interface{}{
		"name":          params.Name,
		"user":          params.UserID,
		"nest":          params.NestID,
		"egg":           params.EggID,
		"docker_image":  params.DockerImage,
		"startup":       "java -Xms128M -Xmx{{SERVER_MEMORY}}M -jar {{SERVER_JARFILE}}",
		"environment": map[string]string{
			"SERVER_JARFILE": "server.jar",
			"VANILLA_VERSION": "latest",
			"BUILD_NUMBER":    "latest",
		},
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

	respBytes, err := c.doRequest("POST", "/api/application/servers", body)
	if err != nil {
		return nil, fmt.Errorf("failed to create server: %w", err)
	}

	var resp ServerResponse
	if err := json.Unmarshal(respBytes, &resp); err != nil {
		return nil, fmt.Errorf("failed to parse server response: %w", err)
	}

	return &resp.Attributes, nil
}