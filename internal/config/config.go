package config

import (
	"fmt"
	"os"
)

type Config struct {
	Port                string
	StripeSecretKey     string
	StripeWebhookSecret string
	PterodactylURL      string
	PterodactylAPIKey   string
	DatabaseURL         string
}

func Load() (*Config, error) {
	cfg := &Config{
		Port:                getEnv("PORT", "8081"),
		StripeSecretKey:     os.Getenv("STRIPE_SECRET_KEY"),
		StripeWebhookSecret: os.Getenv("STRIPE_WEBHOOK_SECRET"),
		PterodactylURL:      os.Getenv("PTERODACTYL_URL"),
		PterodactylAPIKey:   os.Getenv("PTERODACTYL_API_KEY"),
		DatabaseURL:         os.Getenv("DATABASE_URL"),
	}

	if cfg.StripeSecretKey == "" {
		return nil, fmt.Errorf("STRIPE_SECRET_KEY is required")
	}

	return cfg, nil
}

func getEnv(key, fallback string) string {
	if val := os.Getenv(key); val != "" {
		return val
	}
	return fallback
}