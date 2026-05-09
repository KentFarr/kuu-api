package dns

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"
)

type CloudflareClient struct {
	APIToken string
	ZoneID   string
	Domain   string
	HTTP     *http.Client
}

func NewCloudflareClient(token, zoneID, domain string) *CloudflareClient {
	return &CloudflareClient{
		APIToken: token,
		ZoneID:   zoneID,
		Domain:   domain,
		HTTP: &http.Client{
			Timeout: 10 * time.Second,
		},
	}
}

type DNSRecord struct {
	Type    string `json:"type"`
	Name    string `json:"name"`
	Content string `json:"content"`
	TTL     int    `json:"ttl"`
	Proxied bool   `json:"proxied"`
}

type cloudflareResponse struct {
	Success bool     `json:"success"`
	Errors  []errorMsg `json:"errors"`
}

type errorMsg struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

func (c *CloudflareClient) CreateCNAME(ctx context.Context, subdomain, target string) error {
	if c.APIToken == "" || c.ZoneID == "" {
		return fmt.Errorf("cloudflare dns: missing configuration (api token or zone id)")
	}

	record := DNSRecord{
		Type:    "CNAME",
		Name:    fmt.Sprintf("%s.%s", subdomain, c.Domain),
		Content: target,
		TTL:     1,    // Automatic
		Proxied: false, // Gray cloud for game servers
	}

	body, err := json.Marshal(record)
	if err != nil {
		return fmt.Errorf("cloudflare dns: failed to marshal record: %w", err)
	}

	url := fmt.Sprintf("https://api.cloudflare.com/client/v4/zones/%s/dns_records", c.ZoneID)
	req, err := http.NewRequestWithContext(ctx, "POST", url, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("cloudflare dns: failed to create request: %w", err)
	}

	req.Header.Set("Authorization", "Bearer "+c.APIToken)
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.HTTP.Do(req)
	if err != nil {
		return fmt.Errorf("cloudflare dns: request failed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusCreated {
		respBody, _ := io.ReadAll(resp.Body)
		var cfResp cloudflareResponse
		if err := json.Unmarshal(respBody, &cfResp); err == nil && len(cfResp.Errors) > 0 {
			// If record already exists, we consider it a success (idempotency)
			for _, e := range cfResp.Errors {
				if e.Code == 81057 { // Record already exists
					return nil
				}
			}
			return fmt.Errorf("cloudflare dns: api error (status %d): %s", resp.StatusCode, cfResp.Errors[0].Message)
		}
		return fmt.Errorf("cloudflare dns: api error (status %d): %s", resp.StatusCode, string(respBody))
	}

	return nil
}
