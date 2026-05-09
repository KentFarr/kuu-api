package pterodactyl

import (
	"errors"
	"regexp"
	"testing"
)

func TestValidateUsername(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want error
	}{
		{"valid lower", "alice", nil},
		{"valid mixed", "Alice_42", nil},
		{"valid hyphen", "alice-bob", nil},
		{"too short", "ab", ErrUsernameTooShort},
		{"too long", "abcdefghijklmnopqrstuvwxyz0123456", ErrUsernameTooLong},
		{"starts with digit", "1alice", ErrUsernameBadStart},
		{"starts with underscore", "_alice", ErrUsernameBadStart},
		{"starts with hyphen", "-alice", ErrUsernameBadStart},
		{"ends with hyphen", "alice-", ErrUsernameBadEnd},
		{"ends with underscore", "alice_", ErrUsernameBadEnd},
		{"bad chars dot", "alice.bob", ErrUsernameBadChars},
		{"bad chars space", "alice bob", ErrUsernameBadChars},
		{"bad chars at", "alice@bob", ErrUsernameBadChars},
		{"reserved kuu_", "kuu_alice", ErrUsernameReserved},
		{"reserved admin", "adminalice", ErrUsernameReserved},
		{"reserved system", "systembot", ErrUsernameReserved},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := ValidateUsername(tc.in)
			if !errors.Is(got, tc.want) {
				t.Fatalf("ValidateUsername(%q) = %v, want %v", tc.in, got, tc.want)
			}
		})
	}
}

func TestGenerateFriendlyUsernameFormat(t *testing.T) {
	re := regexp.MustCompile(`^[a-z]+-[a-z]+-\d{4}$`)
	for i := 0; i < 50; i++ {
		got := GenerateFriendlyUsername()
		if !re.MatchString(got) {
			t.Fatalf("GenerateFriendlyUsername returned %q, expected adj-noun-NNNN", got)
		}
		if err := ValidateUsername(got); err != nil {
			t.Fatalf("generated username %q failed validation: %v", got, err)
		}
	}
}
