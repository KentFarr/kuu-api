package pterodactyl

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"net/url"
	"regexp"
	"strings"
)

const (
	UsernameMinLen = 3
	UsernameMaxLen = 32
)

var (
	ErrUsernameTooShort     = errors.New("username must be at least 3 characters")
	ErrUsernameTooLong      = errors.New("username must be at most 32 characters")
	ErrUsernameBadStart     = errors.New("username must start with a letter")
	ErrUsernameBadChars     = errors.New("username may only contain letters, numbers, hyphens, and underscores")
	ErrUsernameBadEnd       = errors.New("username must not end with a hyphen or underscore")
	ErrUsernameReserved     = errors.New("username uses a reserved prefix")

	usernameCharRE = regexp.MustCompile(`^[A-Za-z0-9_-]+$`)

	reservedPrefixes = []string{"kuu_", "admin", "system", "root"}

	// Word lists kept short and friendly. Combinations:
	// 60 * 60 * 10000 = 36M, ample headroom against collisions.
	friendlyAdjectives = []string{
		"brave", "calm", "clever", "cosmic", "crimson", "daring", "eager", "electric",
		"fancy", "fierce", "frosty", "gentle", "golden", "happy", "humble", "icy",
		"jolly", "keen", "lively", "lucky", "merry", "mighty", "noble", "nimble",
		"plucky", "proud", "quick", "quiet", "radiant", "rapid", "rustic", "shiny",
		"silent", "silver", "snappy", "spicy", "stoic", "stormy", "sunny", "swift",
		"tidy", "tiny", "tough", "valiant", "velvet", "vivid", "warm", "wild",
		"witty", "young", "zealous", "ancient", "azure", "bold", "breezy", "bright",
		"clever", "cozy", "dainty", "drowsy",
	}
	friendlyNouns = []string{
		"otter", "fox", "wolf", "panda", "tiger", "lion", "bear", "eagle",
		"hawk", "raven", "falcon", "lynx", "owl", "moose", "deer", "rabbit",
		"badger", "ferret", "hedgehog", "koala", "penguin", "puffin", "seal", "whale",
		"dolphin", "shark", "trout", "salmon", "ember", "comet", "nebula", "quasar",
		"nova", "phoenix", "dragon", "kraken", "griffin", "yeti", "sphinx", "pegasus",
		"willow", "cedar", "maple", "birch", "fern", "moss", "river", "stone",
		"summit", "valley", "harbor", "meadow", "thunder", "lantern", "compass", "anchor",
		"falcon", "viper", "cobra", "stallion",
	}
)

// ValidateUsername enforces a conservative subset of Pterodactyl's username
// rules: 3–32 chars, [A-Za-z0-9_-]+, must start with a letter, no trailing
// underscore/hyphen, no reserved prefix. We intentionally reject patterns
// the panel might allow (digits-only, leading underscore) so user-chosen
// names look like usernames, not random tokens.
func ValidateUsername(s string) error {
	s = strings.TrimSpace(s)
	switch {
	case len(s) < UsernameMinLen:
		return ErrUsernameTooShort
	case len(s) > UsernameMaxLen:
		return ErrUsernameTooLong
	}
	first := s[0]
	if !((first >= 'a' && first <= 'z') || (first >= 'A' && first <= 'Z')) {
		return ErrUsernameBadStart
	}
	last := s[len(s)-1]
	if last == '-' || last == '_' {
		return ErrUsernameBadEnd
	}
	if !usernameCharRE.MatchString(s) {
		return ErrUsernameBadChars
	}
	lower := strings.ToLower(s)
	for _, p := range reservedPrefixes {
		if strings.HasPrefix(lower, p) {
			return ErrUsernameReserved
		}
	}
	return nil
}

// GenerateFriendlyUsername returns names like "brave-otter-4821" using
// crypto/rand for indices.
func GenerateFriendlyUsername() string {
	adj := friendlyAdjectives[randIntN(len(friendlyAdjectives))]
	noun := friendlyNouns[randIntN(len(friendlyNouns))]
	num := randIntN(10000)
	return fmt.Sprintf("%s-%s-%04d", adj, noun, num)
}

func randIntN(n int) int {
	if n <= 0 {
		return 0
	}
	v, err := rand.Int(rand.Reader, big.NewInt(int64(n)))
	if err != nil {
		// crypto/rand should not fail on Linux; if it does, fall back to 0
		// rather than panicking — caller will likely retry and eventually
		// hit a real RNG read.
		return 0
	}
	return int(v.Int64())
}

// FindUserByUsername returns the matching Pterodactyl user or (nil, nil) when
// no user has that username. A non-nil error means the lookup itself failed
// and callers should not assume the name is free.
func (c *Client) FindUserByUsername(ctx context.Context, username string) (*UserAttributes, error) {
	path := fmt.Sprintf("/api/application/users?filter[username]=%s", url.QueryEscape(username))
	respBytes, err := c.doRequest(ctx, "GET", path, nil)
	if err != nil {
		return nil, fmt.Errorf("failed to search users: %w", err)
	}

	var resp struct {
		Data []struct {
			Attributes UserAttributes `json:"attributes"`
		} `json:"data"`
	}
	if err := json.Unmarshal(respBytes, &resp); err != nil {
		return nil, fmt.Errorf("failed to parse user search: %w", err)
	}

	// Pterodactyl's filter is a substring match in some versions, so verify
	// the exact username before treating a result as a hit.
	for _, d := range resp.Data {
		if strings.EqualFold(d.Attributes.Username, username) {
			u := d.Attributes
			return &u, nil
		}
	}
	return nil, nil
}
