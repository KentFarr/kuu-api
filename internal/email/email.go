package email

import (
	"context"
	"fmt"
	"log"
	"net"
	"net/mail"
	"net/smtp"
	"strings"
	"time"
)

type Mailer struct {
	Host     string
	Port     string
	Username string
	Password string
	From     string
}

func New(host, port, username, password, from string) *Mailer {
	return &Mailer{
		Host:     host,
		Port:     port,
		Username: username,
		Password: password,
		From:     from,
	}
}

func sanitizeHeader(s string) string {
	return strings.NewReplacer("\r", "", "\n", "").Replace(s)
}

func validRecipient(addr string) bool {
	if strings.ContainsAny(addr, "\r\n") {
		return false
	}
	parsed, err := mail.ParseAddress(addr)
	if err != nil {
		return false
	}
	return parsed.Address == addr
}

// WelcomeData carries everything needed to render the new-server welcome
// email. Decoupling rendering from sending lets the persisted email queue
// store the final subject + body and replay them verbatim on retry.
type WelcomeData struct {
	Username   string
	Password   string
	ServerName string
	Game       string
	Port       int
}

// RenderWelcome produces the (subject, body) pair for a welcome email. The
// body interpolates user-controlled values, but every field that lands in a
// header is run through sanitizeHeader to neutralize CRLF injection.
func RenderWelcome(d WelcomeData) (subject, body string) {
	subject = sanitizeHeader("Your Kuu server is ready")
	body = fmt.Sprintf(`Welcome to Kuu!

Your %s server is live and ready to play.

Panel: https://panel.kuu.gg
Username: %s
Password: %s

Server name: %s
Connect: node1.kuu.gg:%d

You can change your username and password anytime in your Pterodactyl panel
under Account Settings. Please change your password after your first login.

Questions? Join our Discord or reply to this email.

- Kuu`,
		sanitizeHeader(d.Game),
		sanitizeHeader(d.Username),
		d.Password,
		sanitizeHeader(d.ServerName),
		d.Port,
	)
	return subject, body
}

// Ping verifies SMTP reachability with a TCP dial. We deliberately do not
// open an SMTP session — that would burn an authentication round-trip on
// every readiness probe and risk tripping rate limits on the relay.
func (m *Mailer) Ping(ctx context.Context) error {
	deadline, ok := ctx.Deadline()
	timeout := 5 * time.Second
	if ok {
		if remaining := time.Until(deadline); remaining < timeout {
			timeout = remaining
		}
	}
	conn, err := net.DialTimeout("tcp", net.JoinHostPort(m.Host, m.Port), timeout)
	if err != nil {
		return fmt.Errorf("smtp dial failed: %w", err)
	}
	_ = conn.Close()
	return nil
}

// Send transmits an already-rendered message to the given recipient. The
// queue worker calls this directly, so it must be safe to invoke repeatedly
// with identical inputs without state drift.
func (m *Mailer) Send(to, subject, body string) error {
	if !validRecipient(to) {
		return fmt.Errorf("invalid recipient address")
	}

	from := sanitizeHeader(m.From)
	msg := fmt.Sprintf("From: %s\r\nTo: %s\r\nSubject: %s\r\nMIME-Version: 1.0\r\nContent-Type: text/plain; charset=UTF-8\r\n\r\n%s",
		from, sanitizeHeader(to), sanitizeHeader(subject), body)

	auth := smtp.PlainAuth("", m.Username, m.Password, m.Host)
	if err := smtp.SendMail(m.Host+":"+m.Port, auth, m.From, []string{to}, []byte(msg)); err != nil {
		log.Printf("failed to send email to %s: %v", to, err)
		return fmt.Errorf("failed to send email: %w", err)
	}
	log.Printf("email sent to %s", to)
	return nil
}
