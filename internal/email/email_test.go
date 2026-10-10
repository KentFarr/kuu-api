package email

import (
	"strings"
	"testing"
)

func TestValidRecipient(t *testing.T) {
	cases := []struct {
		addr string
		want bool
	}{
		{"plain@example.com", true},
		{"User.Name+tag@example.com", true},
		{"", false},
		{"not-an-email", false},
		// Bare display-name form must be rejected: ParseAddress accepts
		// "Alice <alice@example.com>" but parsed.Address != raw, so we
		// reject it. We only want bare addresses going to SMTP.
		{"Alice <alice@example.com>", false},
		// Header-injection attempts.
		{"alice@example.com\r\nBcc: eve@evil.com", false},
		{"alice@example.com\nBcc: eve@evil.com", false},
	}
	for _, tc := range cases {
		t.Run(tc.addr, func(t *testing.T) {
			if got := validRecipient(tc.addr); got != tc.want {
				t.Fatalf("validRecipient(%q) = %v, want %v", tc.addr, got, tc.want)
			}
		})
	}
}

func TestSanitizeHeaderStripsCRLF(t *testing.T) {
	got := sanitizeHeader("Subject\r\nBcc: eve@evil.com")
	if strings.ContainsAny(got, "\r\n") {
		t.Fatalf("sanitizeHeader must strip CR/LF, got %q", got)
	}
	if got != "SubjectBcc: eve@evil.com" {
		t.Fatalf("unexpected sanitize result: %q", got)
	}
}

func TestRenderWelcomeIncludesAllFields(t *testing.T) {
	subject, body := RenderWelcome(WelcomeData{
		Username:   "kuu_abc123",
		ServerName: "minecraft-2GB-7",
		Game:       "minecraft",
		Port:       25565,
	})
	if subject == "" {
		t.Fatalf("subject must be non-empty")
	}
	for _, want := range []string{"kuu_abc123", "minecraft-2GB-7", "minecraft", "25565", PanelURL} {
		if !strings.Contains(body, want) {
			t.Fatalf("body missing %q:\n%s", want, body)
		}
	}
}

func TestRenderWelcomeSanitizesUserControlledFields(t *testing.T) {
	// If a hostile/garbled username slipped through, the email body should
	// not let it inject extra MIME headers via CRLF. The sanitizer keeps
	// the field appearing in the body but with newlines stripped.
	_, body := RenderWelcome(WelcomeData{
		Username:   "evil\r\nBcc: attacker@example.com",
		ServerName: "ok",
		Game:       "minecraft",
		Port:       1,
	})
	if strings.Contains(body, "Bcc:") && strings.Contains(body, "\r\nBcc:") {
		t.Fatalf("RenderWelcome did not sanitize CRLF-injected username:\n%s", body)
	}
}

func TestBackoffForGrowsAndCaps(t *testing.T) {
	prev := backoffFor(1)
	for i := 2; i < 20; i++ {
		next := backoffFor(i)
		if next < prev {
			t.Fatalf("backoffFor(%d)=%v decreased from backoffFor(%d)=%v", i, next, i-1, prev)
		}
		prev = next
	}
	if backoffFor(1000) != maxBackoff {
		t.Fatalf("backoffFor must cap at maxBackoff (%v), got %v", maxBackoff, backoffFor(1000))
	}
}

func TestRenderWelcomeLoginInstructions(t *testing.T) {
	_, newBody := RenderWelcome(WelcomeData{Username: "u", NewAccount: true})
	if !strings.Contains(newBody, "Setup Your Account") || !strings.Contains(newBody, ForgotPasswordURL) {
		t.Errorf("new-account email must point to the panel's setup email and reset page:\n%s", newBody)
	}

	_, returningBody := RenderWelcome(WelcomeData{Username: "u"})
	if strings.Contains(returningBody, "Setup Your Account") || !strings.Contains(returningBody, "existing Kuu panel account") {
		t.Errorf("returning-customer email should say to use the existing account:\n%s", returningBody)
	}

	// The body is stored in pending_emails; it must never carry a password.
	for _, body := range []string{newBody, returningBody} {
		if strings.Contains(body, "Password:") {
			t.Errorf("email body contains a password line:\n%s", body)
		}
	}
}

func TestRenderPaymentFailed(t *testing.T) {
	subject, body := RenderPaymentFailed("https://invoice.stripe.test/i/1")
	if subject == "" || !strings.Contains(body, "https://invoice.stripe.test/i/1") {
		t.Errorf("subject %q, body missing invoice link:\n%s", subject, body)
	}
}
