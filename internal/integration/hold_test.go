//go:build integration

package integration

import (
	"context"
	"crypto/tls"
	"fmt"
	"io"
	"net"
	"net/http"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/tricknotes/koroku/internal/caddy"
)

// TestCaddyHoldsRequestsWhileAppIsDown runs a real Caddy with the
// generated configuration in front of an app port where nothing listens
// yet, as during the recreation of the app's containers, and checks that a
// GET and a POST with a body wait and reach the app once it listens
// (ARCHITECTURE 8).
func TestCaddyHoldsRequestsWhileAppIsDown(t *testing.T) {
	t.Parallel()
	const image = "caddy:2"
	if err := exec.Command("docker", "image", "inspect", image).Run(); err != nil {
		if out, err := exec.Command("docker", "pull", "--quiet", image).CombinedOutput(); err != nil {
			t.Skipf("the %s image is not available: %v: %s", image, err, out)
		}
	}

	const (
		consoleHost = "console.dev.example.com"
		appHost     = "hold.dev.example.com"
	)
	appPort := freePort(t)
	config, err := caddy.Generate(caddy.Input{
		ConsoleHost: consoleHost,
		// Nothing listens there either: the console answers 502 at once,
		// which tells that Caddy is up.
		ConsoleUpstream: fmt.Sprintf("127.0.0.1:%d", freePort(t)),
		ListenIP:        "127.0.0.1",
		InternalCA:      true,
		Apps:            []caddy.App{{Slug: "hold", HostPort: appPort, Hostnames: []string{appHost}}},
	})
	if err != nil {
		t.Fatal(err)
	}
	httpsPort, caddyLog := startCaddy(t, image, "hold", config)

	client := &http.Client{
		Timeout: time.Minute,
		Transport: &http.Transport{
			DialContext: func(ctx context.Context, network, _ string) (net.Conn, error) {
				return (&net.Dialer{}).DialContext(ctx, network, fmt.Sprintf("127.0.0.1:%d", httpsPort))
			},
			TLSClientConfig: &tls.Config{InsecureSkipVerify: true}, //nolint:gosec // Caddy's local CA
		},
	}
	deadline := time.Now().Add(30 * time.Second)
	for {
		res, err := client.Get("https://" + consoleHost + "/")
		if err == nil {
			_ = res.Body.Close()
			if res.StatusCode == http.StatusBadGateway {
				break
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("caddy did not come up: %v\n%s", err, caddyLog.String())
		}
		time.Sleep(200 * time.Millisecond)
	}

	type result struct {
		status  int
		body    string
		elapsed time.Duration
		err     error
	}
	send := func(method, body string) <-chan result {
		ch := make(chan result, 1)
		go func() {
			start := time.Now()
			var reader io.Reader
			if body != "" {
				reader = strings.NewReader(body)
			}
			req, err := http.NewRequest(method, "https://"+appHost+"/path", reader)
			if err != nil {
				ch <- result{err: err}
				return
			}
			res, err := client.Do(req)
			if err != nil {
				ch <- result{err: err, elapsed: time.Since(start)}
				return
			}
			defer func() { _ = res.Body.Close() }()
			b, _ := io.ReadAll(res.Body)
			ch <- result{status: res.StatusCode, body: string(b), elapsed: time.Since(start)}
		}()
		return ch
	}
	get := send(http.MethodGet, "")
	post := send(http.MethodPost, "payload-of-the-form")

	// The app comes up a while later.
	const downFor = 2 * time.Second
	time.Sleep(downFor)
	l, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", appPort))
	if err != nil {
		t.Fatalf("listen on the app port: %v", err)
	}
	srv := &http.Server{ReadHeaderTimeout: 5 * time.Second, Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		_, _ = fmt.Fprintf(w, "%s %s body=%q", r.Method, r.URL.Path, b)
	})}
	go func() { _ = srv.Serve(l) }()
	t.Cleanup(func() { _ = srv.Close() })

	for name, c := range map[string]struct {
		ch   <-chan result
		want string
	}{
		"GET":  {get, `GET /path body=""`},
		"POST": {post, `POST /path body="payload-of-the-form"`},
	} {
		r := <-c.ch
		switch {
		case r.err != nil:
			t.Errorf("%s: %v\n--- caddy ---\n%s", name, r.err, caddyLog.String())
		case r.status != http.StatusOK || r.body != c.want:
			t.Errorf("%s: %d %q, want 200 %q", name, r.status, r.body, c.want)
		case r.elapsed < downFor-500*time.Millisecond:
			t.Errorf("%s answered after %v, before the app listened", name, r.elapsed)
		}
	}
}
