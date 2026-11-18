package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestRunVersionAndUsage(t *testing.T) {
	var stdout, stderr bytes.Buffer
	if err := run(context.Background(), []string{"version"}, &stdout, &stderr); err != nil {
		t.Fatalf("version: %v", err)
	}
	if strings.TrimSpace(stdout.String()) != version {
		t.Errorf("version output = %q", stdout.String())
	}
	if err := run(context.Background(), nil, &stdout, &stderr); err == nil {
		t.Error("expected an error without a command")
	}
	if err := run(context.Background(), []string{"bogus"}, &stdout, &stderr); err == nil {
		t.Error("expected an error for an unknown command")
	}
	if !strings.Contains(stderr.String(), "Usage: koroku") {
		t.Errorf("usage not printed: %q", stderr.String())
	}
}

func TestServeRejectsMissingConfig(t *testing.T) {
	err := serve(context.Background(), []string{"--config", filepath.Join(t.TempDir(), "none.toml")}, io.Discard, nil)
	if err == nil || !strings.Contains(err.Error(), "read config") {
		t.Fatalf("err = %v", err)
	}
}

// TestServeAnswersSystemEndpoint is the M0 completion check: `koroku serve`
// starts from a config file and GET /api/system responds.
func TestServeAnswersSystemEndpoint(t *testing.T) {
	dir := t.TempDir()
	dataDir := filepath.Join(dir, "data")
	configPath := filepath.Join(dir, "config.toml")
	configText := fmt.Sprintf("base_domain = \"dev.example.com\"\nlisten = \"127.0.0.1:0\"\ndata_dir = %q\nmaster_key_file = %q\n[caddy]\nenabled = false\n",
		dataDir, filepath.Join(dir, "etc", "master.key"))
	if err := os.WriteFile(configPath, []byte(configText), 0o600); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	ready := make(chan string, 1)
	done := make(chan error, 1)
	go func() {
		done <- serve(ctx, []string{"--config", configPath, "--insecure-no-auth"}, io.Discard, ready)
	}()

	var addr string
	select {
	case addr = <-ready:
	case err := <-done:
		t.Fatalf("serve exited early: %v", err)
	case <-time.After(10 * time.Second):
		t.Fatal("serve did not become ready")
	}

	resp, err := http.Get("http://" + addr + "/api/system")
	if err != nil {
		t.Fatalf("GET /api/system: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	var body struct {
		Version    string `json:"version"`
		BaseDomain string `json:"base_domain"`
		Disk       *struct {
			TotalBytes uint64 `json:"total_bytes"`
		} `json:"disk"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if body.Version != version || body.BaseDomain != "dev.example.com" {
		t.Errorf("unexpected body: %+v", body)
	}
	if body.Disk == nil || body.Disk.TotalBytes == 0 {
		t.Errorf("disk usage missing: %+v", body.Disk)
	}
	if _, err := os.Stat(filepath.Join(dataDir, "koroku.db")); err != nil {
		t.Errorf("database was not created: %v", err)
	}
	if info, err := os.Stat(filepath.Join(dir, "etc", "master.key")); err != nil || info.Mode().Perm() != 0o600 {
		t.Errorf("master key was not created with mode 0600: %v", err)
	}

	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Errorf("serve returned %v after shutdown", err)
		}
	case <-time.After(15 * time.Second):
		t.Fatal("serve did not shut down")
	}
}

// TestServeIsLockedWithoutGitHubApp checks the default posture: with no
// GitHub App configured and no explicit opt-out, the API refuses requests.
func TestServeIsLockedWithoutGitHubApp(t *testing.T) {
	dir := t.TempDir()
	configPath := filepath.Join(dir, "config.toml")
	configText := fmt.Sprintf("base_domain = \"dev.example.com\"\nlisten = \"127.0.0.1:0\"\ndata_dir = %q\nmaster_key_file = %q\n[caddy]\nenabled = false\n",
		filepath.Join(dir, "data"), filepath.Join(dir, "master.key"))
	if err := os.WriteFile(configPath, []byte(configText), 0o600); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	ready := make(chan string, 1)
	done := make(chan error, 1)
	go func() { done <- serve(ctx, []string{"--config", configPath}, io.Discard, ready) }()
	var addr string
	select {
	case addr = <-ready:
	case err := <-done:
		t.Fatalf("serve exited early: %v", err)
	case <-time.After(10 * time.Second):
		t.Fatal("serve did not become ready")
	}
	for target, want := range map[string]int{
		"/api/apps":    http.StatusUnauthorized,
		"/api/system":  http.StatusUnauthorized,
		"/api/session": http.StatusOK,
	} {
		resp, err := http.Get("http://" + addr + target)
		if err != nil {
			t.Fatalf("GET %s: %v", target, err)
		}
		_ = resp.Body.Close()
		if resp.StatusCode != want {
			t.Errorf("GET %s = %d, want %d", target, resp.StatusCode, want)
		}
	}
	cancel()
	select {
	case <-done:
	case <-time.After(15 * time.Second):
		t.Fatal("serve did not shut down")
	}
}

func TestGitHubPrivateKeyMustBePrivate(t *testing.T) {
	dir := t.TempDir()
	keyPath := filepath.Join(dir, "github-app.pem")
	if err := os.WriteFile(keyPath, []byte("not a key"), 0o644); err != nil {
		t.Fatal(err)
	}
	configPath := filepath.Join(dir, "config.toml")
	configText := fmt.Sprintf("base_domain = \"dev.example.com\"\nlisten = \"127.0.0.1:0\"\ndata_dir = %q\nmaster_key_file = %q\n[caddy]\nenabled = false\n"+
		"[github]\napp_id = 1\nclient_id = \"id\"\nclient_secret = \"example-secret\"\nallowed_user_id = 42\nprivate_key_file = %q\n",
		filepath.Join(dir, "data"), filepath.Join(dir, "master.key"), keyPath)
	if err := os.WriteFile(configPath, []byte(configText), 0o600); err != nil {
		t.Fatal(err)
	}
	err := serve(context.Background(), []string{"--config", configPath}, io.Discard, nil)
	if err == nil || !strings.Contains(err.Error(), "too open") {
		t.Fatalf("err = %v", err)
	}
	if err := os.Chmod(keyPath, 0o600); err != nil {
		t.Fatal(err)
	}
	err = serve(context.Background(), []string{"--config", configPath}, io.Discard, nil)
	if err == nil || !strings.Contains(err.Error(), "github private key") || strings.Contains(err.Error(), "not a key") {
		t.Fatalf("a malformed key must be rejected without echoing it: %v", err)
	}
}

// TestServeStartsWhileCaddyIsUnreachable: Caddy may come up after koroku (or
// be restarting). koroku must start anyway and log the failed load.
func TestServeStartsWhileCaddyIsUnreachable(t *testing.T) {
	// A port nothing listens on.
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	deadAddr := l.Addr().String()
	_ = l.Close()

	dir := t.TempDir()
	configPath := filepath.Join(dir, "config.toml")
	configText := fmt.Sprintf("base_domain = \"dev.example.com\"\nlisten = \"127.0.0.1:0\"\ndata_dir = %q\nmaster_key_file = %q\n[caddy]\nadmin_url = \"http://%s\"\n",
		filepath.Join(dir, "data"), filepath.Join(dir, "master.key"), deadAddr)
	if err := os.WriteFile(configPath, []byte(configText), 0o600); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	ready := make(chan string, 1)
	done := make(chan error, 1)
	var logs syncBuffer
	go func() { done <- serve(ctx, []string{"--config", configPath}, &logs, ready) }()
	var addr string
	select {
	case addr = <-ready:
	case err := <-done:
		t.Fatalf("serve exited although only Caddy is down: %v", err)
	case <-time.After(30 * time.Second):
		t.Fatal("serve did not become ready")
	}
	resp, err := http.Get("http://" + addr + "/api/session")
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("GET /api/session = %d", resp.StatusCode)
	}
	if !strings.Contains(logs.String(), "loading the Caddy configuration failed") {
		t.Errorf("the failed load was not logged:\n%s", logs.String())
	}
	cancel()
	select {
	case <-done:
	case <-time.After(60 * time.Second):
		t.Fatal("serve did not shut down")
	}
}

// syncBuffer is a bytes.Buffer safe for the concurrent writes of a logger.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// TestInsecureNoAuthIsRefusedBehindCaddy: the development flag must not be
// usable in the configuration that is reachable from the internet.
func TestInsecureNoAuthIsRefusedBehindCaddy(t *testing.T) {
	dir := t.TempDir()
	configPath := filepath.Join(dir, "config.toml")
	configText := fmt.Sprintf("base_domain = \"dev.example.com\"\nlisten = \"127.0.0.1:0\"\ndata_dir = %q\nmaster_key_file = %q\n",
		filepath.Join(dir, "data"), filepath.Join(dir, "master.key"))
	if err := os.WriteFile(configPath, []byte(configText), 0o600); err != nil {
		t.Fatal(err)
	}
	err := serve(context.Background(), []string{"--config", configPath, "--insecure-no-auth"}, io.Discard, nil)
	if err == nil || !strings.Contains(err.Error(), "--insecure-no-auth is refused") {
		t.Fatalf("err = %v", err)
	}
}
