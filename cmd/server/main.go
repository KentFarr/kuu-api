package main

import (
	"fmt"
	"log"
	"net/http"

	"github.com/KentFarr/kuu-api/internal/config"
	"github.com/KentFarr/kuu-api/internal/handlers"
	"github.com/KentFarr/kuu-api/internal/provisioner"
	"github.com/KentFarr/kuu-api/internal/pterodactyl"
)

func main() {
	cfg, err := config.Load()
	if err != nil {
		log.Fatalf("failed to load config: %v", err)
	}

	pteroClient := pterodactyl.NewClient(cfg.PterodactylURL, cfg.PterodactylAPIKey)

	prov := provisioner.New(pteroClient, 1)

	checkout := &handlers.CheckoutHandler{
		StripeKey:  cfg.StripeSecretKey,
		SuccessURL: "https://kuu.gg/success",
		CancelURL:  "https://kuu.gg",
	}

	wh := &handlers.WebhookHandler{
		WebhookSecret: cfg.StripeWebhookSecret,
		Provisioner:   prov,
	}

	mux := http.NewServeMux()
	mux.HandleFunc("GET /health", handlers.HealthCheck)
	mux.HandleFunc("POST /api/checkout", checkout.HandleCheckout)
	mux.HandleFunc("POST /api/webhook", wh.HandleWebhook)

	addr := fmt.Sprintf(":%s", cfg.Port)
	log.Printf("kuu-api starting on %s", addr)

	if err := http.ListenAndServe(addr, mux); err != nil {
		log.Fatalf("server failed: %v", err)
	}
}