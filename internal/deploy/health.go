package deploy

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"time"
)

// HealthChecker confirms that a freshly started app answers HTTP requests.
type HealthChecker interface {
	// Wait polls url until it answers with a status below 500 or the timeout
	// elapses.
	Wait(ctx context.Context, url string, timeout time.Duration) error
}

// HTTPHealthChecker is the real HealthChecker.
type HTTPHealthChecker struct {
	// Interval between attempts. Defaults to one second.
	Interval time.Duration
	// Client defaults to a client with a 5 second timeout that does not
	// follow redirects (a redirect already proves the app is answering).
	Client *http.Client
}

// Wait implements HealthChecker.
func (h HTTPHealthChecker) Wait(ctx context.Context, url string, timeout time.Duration) error {
	interval := h.Interval
	if interval <= 0 {
		interval = time.Second
	}
	client := h.Client
	if client == nil {
		client = &http.Client{
			Timeout: 5 * time.Second,
			CheckRedirect: func(*http.Request, []*http.Request) error {
				return http.ErrUseLastResponse
			},
		}
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	last := "no attempt was made"
	for {
		status, err := probe(ctx, client, url)
		switch {
		case err == nil && status < 500:
			return nil
		case err == nil:
			last = fmt.Sprintf("HTTP %d", status)
		case ctx.Err() == nil:
			// A probe cut short by the deadline says nothing about the app.
			last = "connection failed"
		}
		select {
		case <-ctx.Done():
			if errors.Is(ctx.Err(), context.DeadlineExceeded) {
				return fmt.Errorf("health check did not pass within %s (last result: %s)", timeout, last)
			}
			return ctx.Err()
		case <-time.After(interval):
		}
	}
}

func probe(ctx context.Context, client *http.Client, url string) (int, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return 0, err
	}
	resp, err := client.Do(req)
	if err != nil {
		return 0, err
	}
	_ = resp.Body.Close()
	return resp.StatusCode, nil
}
