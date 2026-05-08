package pterodactyl

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
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

func generatePassword() (string, error) {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("failed to generate password: %w", err)
	}
	return hex.EncodeToString(b), nil
}

func generateUsername(email string) string {
	sum := sha256.Sum256([]byte(email))
	return fmt.Sprintf("kuu_%s", hex.EncodeToString(sum[:])[:12])
}

func (c *Client) CreateUser(ctx context.Context, email string) (*UserAttributes, string, error) {
	password, err := generatePassword()
	if err != nil {
		return nil, "", err
	}

	username := generateUsername(email)

	body := map[string]interface{}{
		"email":      email,
		"username":   username,
		"first_name": "Kuu",
		"last_name":  "Player",
		"password":   password,
	}

	respBytes, err := c.doRequest(ctx, "POST", "/api/application/users", body)
	if err != nil {
		return nil, "", fmt.Errorf("failed to create user: %w", err)
	}

	var resp UserResponse
	if err := json.Unmarshal(respBytes, &resp); err != nil {
		return nil, "", fmt.Errorf("failed to parse user response: %w", err)
	}

	return &resp.Attributes, password, nil
}

func (c *Client) DeleteUser(ctx context.Context, userID int) error {
	path := fmt.Sprintf("/api/application/users/%d", userID)
	_, err := c.doRequest(ctx, "DELETE", path, nil)
	if err != nil {
		return fmt.Errorf("failed to delete user %d: %w", userID, err)
	}
	return nil
}
