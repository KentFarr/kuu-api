package pterodactyl

import (
	"strings"
	"testing"
)

func TestHashUsernameDeterministic(t *testing.T) {
	got1 := hashUsername("alice@example.com")
	got2 := hashUsername("alice@example.com")
	if got1 != got2 {
		t.Fatalf("hashUsername must be deterministic: %q vs %q", got1, got2)
	}
}

func TestHashUsernameDistinctEmailsDistinctUsernames(t *testing.T) {
	a := hashUsername("alice@example.com")
	b := hashUsername("bob@example.com")
	if a == b {
		t.Fatalf("expected distinct usernames for distinct emails, got %q for both", a)
	}
}

func TestHashUsernamePrefixAndLength(t *testing.T) {
	got := hashUsername("anyone@example.com")
	if !strings.HasPrefix(got, "kuu_") {
		t.Fatalf("username must start with kuu_, got %q", got)
	}
	if len(got) > 32 {
		t.Fatalf("username too long for pterodactyl: %d chars (%q)", len(got), got)
	}
}

func TestHashUsernameOnEmptyEmail(t *testing.T) {
	got := hashUsername("")
	if !strings.HasPrefix(got, "kuu_") || len(got) != 16 {
		t.Fatalf("unexpected username for empty email: %q", got)
	}
}

func TestGeneratePasswordLengthAndUniqueness(t *testing.T) {
	a, err := GeneratePassword()
	if err != nil {
		t.Fatalf("GeneratePassword failed: %v", err)
	}
	if len(a) != 32 {
		t.Fatalf("expected 32-char hex password (16 bytes), got %d (%q)", len(a), a)
	}
	b, err := GeneratePassword()
	if err != nil {
		t.Fatalf("GeneratePassword failed: %v", err)
	}
	if a == b {
		t.Fatalf("two random passwords should not collide: %q == %q", a, b)
	}
}
