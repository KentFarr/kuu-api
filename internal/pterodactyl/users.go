package pterodactyl

import (
	"context"
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

// UsernameSource records which path produced the final username so callers
// can log it for observability — drift between user-chosen and generated
// names is operationally interesting.
type UsernameSource string

const (
	UsernameSourceUserChosen   UsernameSource = "user_chosen"
	UsernameSourceGenerated    UsernameSource = "generated"
	UsernameSourceHashFallback UsernameSource = "hash_fallback"
)

const friendlyGenerateAttempts = 5

// hashUsername is the legacy fallback: deterministic, guaranteed unique per
// email, but ugly. Used only when friendly generation collides repeatedly.
func hashUsername(email string) string {
	sum := sha256.Sum256([]byte(email))
	return fmt.Sprintf("kuu_%s", hex.EncodeToString(sum[:])[:12])
}

// pickUsername runs the fallback chain: user-chosen → friendly → hash. It
// returns the chosen username, the source label, and any non-fatal lookup
// error so the caller can log it.
func (c *Client) pickUsername(ctx context.Context, email, requested string) (string, UsernameSource, error) {
	if requested != "" {
		if err := ValidateUsername(requested); err == nil {
			existing, lookupErr := c.FindUserByUsername(ctx, requested)
			if lookupErr != nil {
				// Treat a panel lookup failure as "not safe to use this name"
				// — we'd rather generate a friendly fallback than collide.
				return c.pickGeneratedUsername(ctx, email, lookupErr)
			}
			if existing == nil {
				return requested, UsernameSourceUserChosen, nil
			}
		}
	}
	return c.pickGeneratedUsername(ctx, email, nil)
}

func (c *Client) pickGeneratedUsername(ctx context.Context, email string, prior error) (string, UsernameSource, error) {
	for i := 0; i < friendlyGenerateAttempts; i++ {
		candidate := GenerateFriendlyUsername()
		existing, err := c.FindUserByUsername(ctx, candidate)
		if err != nil {
			prior = err
			continue
		}
		if existing == nil {
			return candidate, UsernameSourceGenerated, prior
		}
	}
	return hashUsername(email), UsernameSourceHashFallback, prior
}

// CreateUser creates a panel account without a password. Pterodactyl then
// emails the user an "Account Created" message with a one-time link to set
// their own password (valid for 60 minutes; after that, the panel's
// "Forgot password" page issues a new one). We never see or store the
// password.
func (c *Client) CreateUser(ctx context.Context, email, requestedUsername string) (*UserAttributes, UsernameSource, error) {
	username, source, lookupErr := c.pickUsername(ctx, email, requestedUsername)
	_ = lookupErr // surfaced via source==hash_fallback or generated; caller logs source

	body := map[string]interface{}{
		"email":      email,
		"username":   username,
		"first_name": "Kuu",
		"last_name":  "Player",
	}

	respBytes, err := c.doRequest(ctx, "POST", "/api/application/users", body)
	if err != nil {
		return nil, "", fmt.Errorf("failed to create user: %w", err)
	}

	var resp UserResponse
	if err := json.Unmarshal(respBytes, &resp); err != nil {
		return nil, "", fmt.Errorf("failed to parse user response: %w", err)
	}

	return &resp.Attributes, source, nil
}

func (c *Client) DeleteUser(ctx context.Context, userID int) error {
	path := fmt.Sprintf("/api/application/users/%d", userID)
	_, err := c.doRequest(ctx, "DELETE", path, nil)
	if err != nil {
		return fmt.Errorf("failed to delete user %d: %w", userID, err)
	}
	return nil
}
