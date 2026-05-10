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
	ID      string `json:"id,omitempty"`
	Type    string `json:"type"`
	Name    string `json:"name"`
	Content string `json:"content"`
	TTL     int    `json:"ttl"`
	Proxied bool   `json:"proxied"`
}

type cloudflareResponse struct {
	Success bool        `json:"success"`
	Errors  []errorMsg  `json:"errors"`
	Result  []DNSRecord `json:"result"`
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

func (c *CloudflareClient) DeleteCNAME(ctx context.Context, subdomain string) error {
	if c.APIToken == "" || c.ZoneID == "" {
		return fmt.Errorf("cloudflare dns: missing configuration (api token or zone id)")
	}

	fullName := fmt.Sprintf("%s.%s", subdomain, c.Domain)

	// 1. Find the record ID
	url := fmt.Sprintf("https://api.cloudflare.com/client/v4/zones/%s/dns_records?name=%s&type=CNAME", c.ZoneID, fullName)
	req, err := http.NewRequestWithContext(ctx, "GET", url, nil)
	if err != nil {
		return fmt.Errorf("cloudflare dns: failed to create lookup request: %w", err)
	}

	req.Header.Set("Authorization", "Bearer "+c.APIToken)
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.HTTP.Do(req)
	if err != nil {
		return fmt.Errorf("cloudflare dns: lookup request failed: %w", err)
	}
	defer resp.Body.Close()

	respBody, _ := io.ReadAll(resp.Body)
	var cfResp cloudflareResponse
	if err := json.Unmarshal(respBody, &cfResp); err != nil {
		return fmt.Errorf("cloudflare dns: failed to parse lookup response: %w", err)
	}

	if !cfResp.Success {
		msg := "unknown error"
		if len(cfResp.Errors) > 0 {
			msg = cfResp.Errors[0].Message
		}
		return fmt.Errorf("cloudflare dns: lookup api error: %s", msg)
	}

	if len(cfResp.Result) == 0 {
		// Record already gone? Success (idempotency).
		return nil
	}

	recordID := cfResp.Result[0].ID

	// 2. Delete the record
	url = fmt.Sprintf("https://api.cloudflare.com/client/v4/zones/%s/dns_records/%s", c.ZoneID, recordID)
	req, err = http.NewRequestWithContext(ctx, "DELETE", url, nil)
	if err != nil {
		return fmt.Errorf("cloudflare dns: failed to create delete request: %w", err)
	}

	req.Header.Set("Authorization", "Bearer "+c.APIToken)
	req.Header.Set("Content-Type", "application/json")

	resp, err = c.HTTP.Do(req)
	if err != nil {
		return fmt.Errorf("cloudflare dns: delete request failed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		respBody, _ := io.ReadAll(resp.Body)
		var delResp cloudflareResponse
		if err := json.Unmarshal(respBody, &delResp); err == nil && len(delResp.Errors) > 0 {
			return fmt.Errorf("cloudflare dns: delete api error (status %d): %s", resp.StatusCode, delResp.Errors[0].Message)
		}
		return fmt.Errorf("cloudflare dns: delete api error (status %d): %s", resp.StatusCode, string(respBody))
	}

	return nil
}
