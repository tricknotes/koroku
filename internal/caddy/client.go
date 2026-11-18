package caddy

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// DefaultAdminURL is the admin API endpoint used when Client.AdminURL is empty.
const DefaultAdminURL = "http://" + DefaultAdminListen

// DefaultLoadTimeout bounds one Load call when Client.Timeout is zero.
const DefaultLoadTimeout = 30 * time.Second

// maxErrorBody is how much of an error response is kept in the returned error.
const maxErrorBody = 512

// Loader replaces the running Caddy configuration.
type Loader interface {
	Load(ctx context.Context, config []byte) error
}

// Client talks to the Caddy admin API. The zero value is usable.
type Client struct {
	// AdminURL is the base URL of the admin API. Empty means DefaultAdminURL.
	AdminURL string
	// HTTP is the client used for requests. Nil means http.DefaultClient.
	HTTP *http.Client
	// Timeout bounds one Load call in addition to any deadline on the
	// context. Zero means DefaultLoadTimeout.
	Timeout time.Duration
}

var _ Loader = (*Client)(nil)

// StatusError is returned by Load when the admin API answers with a non-2xx status.
type StatusError struct {
	StatusCode int
	// Body is the beginning of the response body, usually a JSON object with
	// an "error" field explaining why the configuration was rejected.
	Body string
}

func (e *StatusError) Error() string {
	if e.Body == "" {
		return fmt.Sprintf("unexpected status %d", e.StatusCode)
	}
	return fmt.Sprintf("unexpected status %d: %s", e.StatusCode, e.Body)
}

// Load replaces the whole running configuration with config through
// POST <AdminURL>/load. Caddy keeps the previous configuration when the new
// one is rejected, and does nothing when config equals the running one.
func (c *Client) Load(ctx context.Context, config []byte) error {
	timeout := c.Timeout
	if timeout <= 0 {
		timeout = DefaultLoadTimeout
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	base := c.AdminURL
	if base == "" {
		base = DefaultAdminURL
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimRight(base, "/")+"/load", bytes.NewReader(config))
	if err != nil {
		return fmt.Errorf("caddy load: build request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")

	httpClient := c.HTTP
	if httpClient == nil {
		httpClient = http.DefaultClient
	}
	resp, err := httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("caddy load: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, maxErrorBody+1))
		return fmt.Errorf("caddy load: %w", &StatusError{StatusCode: resp.StatusCode, Body: excerpt(body)})
	}
	// Drain so the connection can be reused.
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, maxErrorBody))
	return nil
}

// excerpt turns the beginning of a response body into a single line.
func excerpt(body []byte) string {
	truncated := len(body) > maxErrorBody
	if truncated {
		body = body[:maxErrorBody]
	}
	s := strings.Join(strings.Fields(strings.ToValidUTF8(string(body), "")), " ")
	if truncated {
		s += "..."
	}
	return s
}
