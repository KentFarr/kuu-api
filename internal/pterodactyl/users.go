package pterodactyl

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
)

type UserAttributes struct {
	ID       int    `json:"id"`
	Username string `json:"username"`
	Email    string `json:"email"`
}

type UserResponse struct {
	Attributes UserAttributes `json:"attributes"`
}

func generatePassword() string {
	bytes := make([]byte, 16)
	rand.Read(bytes)
	return hex.EncodeToString(bytes)
}

func (c *Client) CreateUser(email string) (*UserAttributes, string, error) {
	password := generatePassword()

	username := fmt.Sprintf("kuu_%s", email[:8])

	body := map[string]interface{}{
		"email":      email,
		"username":   username,
		"first_name": "Kuu",
		"last_name":  "Player",
		"password":   password,
	}

	respBytes, err := c.doRequest("POST", "/api/application/users", body)
	if err != nil {
		return nil, "", fmt.Errorf("failed to create user: %w", err)
	}

	var resp UserResponse
	if err := json.Unmarshal(respBytes, &resp); err != nil {
		return nil, "", fmt.Errorf("failed to parse user response: %w", err)
	}

	return &resp.Attributes, password, nil
}