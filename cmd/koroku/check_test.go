package main

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// A config straight from the example fails the check and says what to fix,
// without reaching any external service.
func TestCheckReportsAnUnconfiguredHost(t *testing.T) {
	dir := t.TempDir()
	configPath := filepath.Join(dir, "config.toml")
	text := "base_domain = \"dev.example.com\"\npublic_ip = \"203.0.113.10\"\n" +
		"data_dir = \"" + filepath.Join(dir, "data") + "\"\nmaster_key_file = \"" + filepath.Join(dir, "master.key") + "\"\n" +
		"[caddy]\nenabled = false\n[backup]\nrestic_password_file = \"" + filepath.Join(dir, "restic.key") + "\"\n"
	if err := os.WriteFile(configPath, []byte(text), 0o600); err != nil {
		t.Fatal(err)
	}
	var stdout, stderr bytes.Buffer
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // DNS lookups and HTTP requests fail at once
	err := run(ctx, []string{"check", "--config", configPath}, &stdout, &stderr)
	if err == nil {
		t.Fatal("check passed on an example config")
	}
	out := stdout.String()
	for _, want := range []string{
		"[FAIL] config", "public_ip",
		"[FAIL] github.app", "nobody can log in",
		"[FAIL] storage", "[skip] backup",
		"[warn] master_key",
		"docs/SETUP.md",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("output lacks %q:\n%s", want, out)
		}
	}
}

func TestCheckReportsABrokenConfig(t *testing.T) {
	configPath := filepath.Join(t.TempDir(), "config.toml")
	if err := os.WriteFile(configPath, []byte("base_domain = \"\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	var stdout bytes.Buffer
	err := run(context.Background(), []string{"check", "--config", configPath}, &stdout, &bytes.Buffer{})
	if err == nil || !strings.Contains(stdout.String(), "base_domain is required") {
		t.Fatalf("err = %v, output %q", err, stdout.String())
	}
}
