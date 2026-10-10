// Command fakeptero is a stand-in for the Pterodactyl panel, for the local
// demo (make demo). It keeps everything in memory and serves only the
// Application API endpoints kuu-api uses. It is not for production.
//
// When SMTP_ADDR is set, it emails the "Account Created" set-password
// message the real panel sends, so the demo's mail viewer shows both
// emails a new customer receives.
package main

import (
	"flag"
	"fmt"
	"log"
	"net/http"
	"net/smtp"
	"os"
	"time"

	"github.com/KentFarr/kuu-api/internal/plans"
	"github.com/KentFarr/kuu-api/internal/pterodactyl"
)

func main() {
	addr := flag.String("addr", envOr("FAKEPTERO_ADDR", ":8090"), "listen address")
	nodeID := flag.Int("node-id", 1, "ID of the single fake node")
	fqdn := flag.String("fqdn", envOr("FAKEPTERO_NODE_FQDN", "node1.demo.local"), "node FQDN players connect to")
	smtpAddr := flag.String("smtp", os.Getenv("SMTP_ADDR"), "SMTP server for setup emails (host:port); empty disables them")
	flag.Parse()

	// 64 GB / 1 TB node with the first 20 ports of every game's range.
	var ranges []portRange
	for _, g := range plans.Games {
		ranges = append(ranges, portRange{g.MinPort, min(g.MinPort+19, g.MaxPort)})
	}
	p := newPanel(*nodeID, *fqdn, 64*1024, 1024*1024, ranges)

	if *smtpAddr != "" {
		p.onUserCreated = func(u pterodactyl.UserAttributes) {
			go sendSetupEmail(*smtpAddr, u)
		}
	}

	log.Printf("fakeptero: node %d (%s) listening on %s", *nodeID, *fqdn, *addr)
	srv := &http.Server{Addr: *addr, Handler: p.routes(), ReadHeaderTimeout: 10 * time.Second}
	log.Fatal(srv.ListenAndServe())
}

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

// sendSetupEmail mimics the real panel's AccountCreated notification.
func sendSetupEmail(addr string, u pterodactyl.UserAttributes) {
	msg := fmt.Sprintf("From: panel@demo.local\r\nTo: %s\r\nSubject: Account Created\r\n\r\n"+
		"Hello %s!\r\n\r\nYou are receiving this email because an account has been created for you on the Pterodactyl panel.\r\n\r\n"+
		"Username: %s\r\nEmail: %s\r\n\r\n"+
		"Setup Your Account: http://panel.demo.local/auth/password/reset/demo-token?email=%s\r\n\r\n"+
		"(This is the fake panel used by the kuu-api demo; the link goes nowhere.)\r\n",
		u.Email, u.Username, u.Username, u.Email, u.Email)
	if err := smtp.SendMail(addr, nil, "panel@demo.local", []string{u.Email}, []byte(msg)); err != nil {
		log.Printf("fakeptero: setup email to %s failed: %v", u.Email, err)
		return
	}
	log.Printf("fakeptero: sent Account Created setup email to %s", u.Email)
}
