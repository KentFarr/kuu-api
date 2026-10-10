// Command demo-checkout sends kuu-api a signed checkout.session.completed
// event, the way Stripe does after a customer pays, so the local demo runs
// the real webhook → provisioning path without a Stripe account.
//
//	go run ./cmd/demo-checkout -game minecraft -email you@example.com
//	go run ./cmd/demo-checkout -repeat   # same event twice: shows idempotency
//
// It signs with the demo webhook secret from .env.demo. Against real Stripe
// test mode, use `stripe listen` and `stripe trigger` instead (see README).
package main

import (
	"bytes"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/stripe/stripe-go/v82/webhook"

	"github.com/KentFarr/kuu-api/internal/plans"
)

const demoWebhookSecret = "whsec_demo_not_a_real_secret" //nolint:gosec // G101: published demo value, matches .env.demo

func main() {
	url := flag.String("url", "http://localhost:8081/api/webhook", "kuu-api webhook endpoint")
	secret := flag.String("secret", envOr("STRIPE_WEBHOOK_SECRET", demoWebhookSecret), "webhook signing secret")
	game := flag.String("game", "minecraft", "game key: "+strings.Join(gameKeys(), ", "))
	variant := flag.String("variant", "", "variant key (empty = the game's default)")
	version := flag.String("version", "", "game version (empty = the variant's default)")
	plan := flag.String("plan", "8GB", "plan: 2GB, 4GB, 8GB, 16GB, 24GB or 32GB")
	email := flag.String("email", "demo@example.com", "customer email")
	username := flag.String("username", "", "panel username typed at checkout (optional)")
	subdomain := flag.String("subdomain", "", "subdomain (8GB plans and up)")
	repeat := flag.Bool("repeat", false, "deliver the same event twice to show idempotency")
	flag.Parse()

	if _, ok := plans.Games[*game]; !ok {
		fail("unknown game %q (choose from %s)", *game, strings.Join(gameKeys(), ", "))
	}
	priceID := "price_demo_" + strings.ToLower(*plan) // matches .env.demo

	// Unique IDs per run, like a fresh purchase.
	stamp := time.Now().Format("20060102150405")
	session := map[string]any{
		"id":               "cs_demo_" + stamp,
		"object":           "checkout.session",
		"mode":             "subscription",
		"status":           "complete",
		"customer":         "cus_demo_" + stamp,
		"subscription":     "sub_demo_" + stamp,
		"customer_details": map[string]any{"email": *email},
		"metadata": map[string]string{
			"game":      *game,
			"variant":   *variant,
			"version":   *version,
			"subdomain": *subdomain,
			"price_id":  priceID,
		},
	}
	if *username != "" {
		session["custom_fields"] = []map[string]any{
			{"key": "username", "type": "text", "text": map[string]any{"value": *username}},
		}
	}
	event, err := json.Marshal(map[string]any{
		"id":          "evt_demo_" + stamp,
		"object":      "event",
		"type":        "checkout.session.completed",
		"api_version": "2025-03-31.basil",
		"created":     time.Now().Unix(),
		"data":        map[string]any{"object": session},
	})
	if err != nil {
		fail("encode event: %v", err)
	}

	deliveries := 1
	if *repeat {
		deliveries = 2
	}
	fmt.Printf("checkout.session.completed evt_demo_%s: %s (%s plan) for %s\n", stamp, *game, *plan, *email)
	for i := 1; i <= deliveries; i++ {
		status, body := send(*url, *secret, event)
		fmt.Printf("  delivery %d → %s %s\n", i, status, strings.TrimSpace(body))
	}
	if *repeat {
		fmt.Println("The second delivery was acknowledged without provisioning again (see \"event already processed, skipping\" in the kuu-api logs).")
	}
}

// send signs the payload exactly as Stripe does (Stripe-Signature header
// with a timestamped HMAC) and posts it.
func send(url, secret string, payload []byte) (string, string) {
	signed := webhook.GenerateTestSignedPayload(&webhook.UnsignedPayload{Payload: payload, Secret: secret})
	req, err := http.NewRequest(http.MethodPost, url, bytes.NewReader(payload))
	if err != nil {
		fail("build request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Stripe-Signature", signed.Header)

	client := &http.Client{Timeout: 60 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		fail("POST %s: %v (is the demo running? try `make demo`)", url, err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	return resp.Status, string(body)
}

func gameKeys() []string {
	keys := make([]string, 0, len(plans.Games))
	for k := range plans.Games {
		keys = append(keys, k)
	}
	return keys
}

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

func fail(format string, args ...any) {
	fmt.Fprintf(os.Stderr, "demo-checkout: "+format+"\n", args...)
	os.Exit(1)
}
