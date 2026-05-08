package pterodactyl

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"

	"github.com/KentFarr/kuu-api/internal/metrics"
)

type Client struct {
	BaseURL    string
	APIKey     string
	HTTPClient *http.Client
}

type APIError struct {
	StatusCode int
	Body       string
}

func (e *APIError) Error() string {
	if e.Body != "" {
		return fmt.Sprintf("pterodactyl API error (status %d): %s", e.StatusCode, e.Body)
	}
	return fmt.Sprintf("pterodactyl API error (status %d)", e.StatusCode)
}

func NewClient(baseURL, apiKey string) *Client {
	return &Client{
		BaseURL: baseURL,
		APIKey:  apiKey,
		HTTPClient: &http.Client{
			Timeout: 30 * time.Second,
		},
	}
}

func (c *Client) doRequest(ctx context.Context, method, path string, body interface{}) ([]byte, error) {
	start := time.Now()
	route := routeLabel(method, path)
	outcome := metrics.OutcomeOK
	defer func() {
		metrics.PterodactylRequestDuration.
			WithLabelValues(route, outcome).
			Observe(time.Since(start).Seconds())
	}()

	var reqBody io.Reader
	if body != nil {
		jsonBytes, err := json.Marshal(body)
		if err != nil {
			outcome = metrics.OutcomeError
			return nil, fmt.Errorf("failed to marshal request body: %w", err)
		}
		reqBody = bytes.NewBuffer(jsonBytes)
	}

	url := fmt.Sprintf("%s%s", c.BaseURL, path)
	req, err := http.NewRequestWithContext(ctx, method, url, reqBody)
	if err != nil {
		outcome = metrics.OutcomeError
		return nil, fmt.Errorf("failed to create request: %w", err)
	}

	req.Header.Set("Authorization", "Bearer "+c.APIKey)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")

	resp, err := c.HTTPClient.Do(req)
	if err != nil {
		outcome = metrics.OutcomeError
		return nil, fmt.Errorf("request failed: %w", err)
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		outcome = metrics.OutcomeError
		return nil, fmt.Errorf("failed to read response: %w", err)
	}

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		outcome = metrics.OutcomeError
		return nil, &APIError{StatusCode: resp.StatusCode, Body: string(respBody)}
	}

	return respBody, nil
}

// routeLabel collapses path IDs to a stable label so the metric cardinality
// stays low. /api/application/servers/123 → "POST /api/application/servers/:id".
// We only emit a small fixed set of labels because Prometheus cardinality
// blows up if every panel server ID gets its own time series.
func routeLabel(method, path string) string {
	// Strip query string.
	if i := indexByteFast(path, '?'); i >= 0 {
		path = path[:i]
	}
	collapsed := collapseNumericSegments(path)
	return method + " " + collapsed
}

func indexByteFast(s string, b byte) int {
	for i := 0; i < len(s); i++ {
		if s[i] == b {
			return i
		}
	}
	return -1
}

func collapseNumericSegments(path string) string {
	out := make([]byte, 0, len(path))
	i := 0
	for i < len(path) {
		if path[i] != '/' {
			out = append(out, path[i])
			i++
			continue
		}
		out = append(out, '/')
		i++
		// Read next segment.
		j := i
		allDigits := j < len(path) && path[j] != '/'
		for j < len(path) && path[j] != '/' {
			if path[j] < '0' || path[j] > '9' {
				allDigits = false
			}
			j++
		}
		if allDigits && j > i {
			out = append(out, ':', 'i', 'd')
		} else {
			out = append(out, path[i:j]...)
		}
		i = j
	}
	return string(out)
}

// Ping issues a lightweight authenticated request to confirm the panel is
// reachable and our API key is still valid. Used by the readiness probe so
// the orchestrator can stop sending traffic when Pterodactyl is degraded.
func (c *Client) Ping(ctx context.Context) error {
	_, err := c.doRequest(ctx, http.MethodGet, "/api/application/nodes?per_page=1", nil)
	return err
}
