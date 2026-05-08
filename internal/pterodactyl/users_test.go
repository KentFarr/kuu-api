package pterodactyl

import (
	"strings"
	"testing"
)

func TestGenerateUsernameDeterministic(t *testing.T) {
	got1 := generateUsername("alice@example.com")
	got2 := generateUsername("alice@example.com")
	if got1 != got2 {
		t.Fatalf("generateUsername must be deterministic: %q vs %q", got1, got2)
	}
}

func TestGenerateUsernameDistinctEmailsDistinctUsernames(t *testing.T) {
	a := generateUsername("alice@example.com")
	b := generateUsername("bob@example.com")
	if a == b {
		t.Fatalf("expected distinct usernames for distinct emails, got %q for both", a)
	}
}

func TestGenerateUsernamePrefixAndLength(t *testing.T) {
	got := generateUsername("anyone@example.com")
	if !strings.HasPrefix(got, "kuu_") {
		t.Fatalf("username must start with kuu_, got %q", got)
	}
	// Pterodactyl rejects usernames longer than 32 chars; ours is "kuu_" + 12 hex = 16.
	if len(got) > 32 {
		t.Fatalf("username too long for pterodactyl: %d chars (%q)", len(got), got)
	}
}

func TestGenerateUsernameOnEmptyEmail(t *testing.T) {
	// Even an empty input must not panic and must yield a syntactically
	// valid username — sha256 of "" is well-defined.
	got := generateUsername("")
	if !strings.HasPrefix(got, "kuu_") || len(got) != 16 {
		t.Fatalf("unexpected username for empty email: %q", got)
	}
}

func TestGeneratePasswordLengthAndUniqueness(t *testing.T) {
	a, err := generatePassword()
	if err != nil {
		t.Fatalf("generatePassword failed: %v", err)
	}
	if len(a) != 32 {
		t.Fatalf("expected 32-char hex password (16 bytes), got %d (%q)", len(a), a)
	}
	b, err := generatePassword()
	if err != nil {
		t.Fatalf("generatePassword failed: %v", err)
	}
	if a == b {
		t.Fatalf("two random passwords should not collide: %q == %q", a, b)
	}
}
