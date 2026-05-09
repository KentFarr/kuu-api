package config

import (
	"fmt"
	"net"
	"os"
	"strconv"
	"strings"

	"github.com/joho/godotenv"
)

type Config struct {
	Port                string
	StripeSecretKey     string
	StripeWebhookSecret string
	PterodactylURL      string
	PterodactylAPIKey   string
	DatabaseURL         string
	SMTPHost            string
	SMTPPort            string
	SMTPUsername        string
	SMTPPassword        string
	SMTPFrom            string
	NodeIDs             []int
	CheckoutSuccessURL  string
	CheckoutCancelURL   string
	CORSAllowedOrigin   string
	TrustedProxyCIDRs   []*net.IPNet
	AdminBearerToken    string
	StripeFirstMonthCouponID string
	CloudflareAPIToken  string
	CloudflareZoneID    string
	CloudflareDomain    string
}

func Load() (*Config, error) {
	// godotenv.Load() is a no-op when .env is absent, which is exactly what
	// we want in prod — env vars come from the orchestrator's secret store
	// (k8s Secret, systemd EnvironmentFile, SSM, etc.) and no .env file is
	// present on disk. See .env.example for the operational guidance.
	godotenv.Load()

	nodeIDs, err := parseNodeIDs(os.Getenv("NODE_IDS"), os.Getenv("NODE_ID"))
	if err != nil {
		return nil, err
	}

	trustedProxies, err := parseCIDRs(os.Getenv("TRUSTED_PROXY_CIDRS"))
	if err != nil {
		return nil, fmt.Errorf("TRUSTED_PROXY_CIDRS: %w", err)
	}

	cfg := &Config{
		Port:                getEnv("PORT", "8081"),
		StripeSecretKey:     os.Getenv("STRIPE_SECRET_KEY"),
		StripeWebhookSecret: os.Getenv("STRIPE_WEBHOOK_SECRET"),
		PterodactylURL:      os.Getenv("PTERODACTYL_URL"),
		PterodactylAPIKey:   os.Getenv("PTERODACTYL_API_KEY"),
		DatabaseURL:         os.Getenv("DATABASE_URL"),
		SMTPHost:            os.Getenv("SMTP_HOST"),
		SMTPPort:            getEnv("SMTP_PORT", "587"),
		SMTPUsername:        os.Getenv("SMTP_USERNAME"),
		SMTPPassword:        os.Getenv("SMTP_PASSWORD"),
		SMTPFrom:            getEnv("SMTP_FROM", "noreply@kuu.gg"),
		NodeIDs:             nodeIDs,
		CheckoutSuccessURL:  getEnv("CHECKOUT_SUCCESS_URL", "https://kuu.gg/success"),
		CheckoutCancelURL:   getEnv("CHECKOUT_CANCEL_URL", "https://kuu.gg"),
		CORSAllowedOrigin:   getEnv("CORS_ALLOWED_ORIGIN", "https://kuu.gg"),
		TrustedProxyCIDRs:   trustedProxies,
		AdminBearerToken:    os.Getenv("ADMIN_BEARER_TOKEN"),
		StripeFirstMonthCouponID: os.Getenv("STRIPE_FIRST_MONTH_COUPON_ID"),
		CloudflareAPIToken:  os.Getenv("CLOUDFLARE_API_TOKEN"),
		CloudflareZoneID:    os.Getenv("CLOUDFLARE_ZONE_ID"),
		CloudflareDomain:    getEnv("CLOUDFLARE_DOMAIN", "kuu.gg"),
	}

	required := map[string]string{
		"STRIPE_SECRET_KEY":     cfg.StripeSecretKey,
		"STRIPE_WEBHOOK_SECRET": cfg.StripeWebhookSecret,
		"PTERODACTYL_URL":       cfg.PterodactylURL,
		"PTERODACTYL_API_KEY":   cfg.PterodactylAPIKey,
		"DATABASE_URL":          cfg.DatabaseURL,
	}
	for name, value := range required {
		if value == "" {
			return nil, fmt.Errorf("%s is required", name)
		}
	}

	return cfg, nil
}

func getEnv(key, fallback string) string {
	if val := os.Getenv(key); val != "" {
		return val
	}
	return fallback
}

// parseNodeIDs reads the multi-node pool from NODE_IDS (comma-separated),
// falling back to a single-element slice from legacy NODE_ID. Returns an
// error if neither is set, if any entry is non-integer, or if duplicates
// are present (a duplicate node would skew load-based selection).
func parseNodeIDs(rawList, legacy string) ([]int, error) {
	rawList = strings.TrimSpace(rawList)
	if rawList == "" {
		legacy = strings.TrimSpace(legacy)
		if legacy == "" {
			legacy = "1"
		}
		id, err := strconv.Atoi(legacy)
		if err != nil {
			return nil, fmt.Errorf("NODE_ID must be an integer: %w", err)
		}
		return []int{id}, nil
	}
	seen := make(map[int]struct{})
	var ids []int
	for _, part := range strings.Split(rawList, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		id, err := strconv.Atoi(part)
		if err != nil {
			return nil, fmt.Errorf("NODE_IDS contains non-integer %q: %w", part, err)
		}
		if _, dup := seen[id]; dup {
			return nil, fmt.Errorf("NODE_IDS contains duplicate node %d", id)
		}
		seen[id] = struct{}{}
		ids = append(ids, id)
	}
	if len(ids) == 0 {
		return nil, fmt.Errorf("NODE_IDS is set but contains no valid entries")
	}
	return ids, nil
}

func parseCIDRs(raw string) ([]*net.IPNet, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil, nil
	}
	var nets []*net.IPNet
	for _, part := range strings.Split(raw, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		_, n, err := net.ParseCIDR(part)
		if err != nil {
			return nil, fmt.Errorf("invalid CIDR %q: %w", part, err)
		}
		nets = append(nets, n)
	}
	return nets, nil
}
