package main

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/tricknotes/koroku/internal/config"
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

func TestInstallationAccountFilter(t *testing.T) {
	both := config.GitHub{AllowedUserID: 42, InstallationAccountIDs: []int64{2002}, InstallationAccounts: []string{" My-Org "}}
	idsOnly := config.GitHub{AllowedUserID: 42, InstallationAccountIDs: []int64{2002}}
	loginsOnly := config.GitHub{AllowedUserID: 42, InstallationAccounts: []string{" My-Org "}}
	for _, tc := range []struct {
		name  string
		cfg   config.GitHub
		id    int64
		login string
		want  bool
	}{
		{"the operator's own account, whatever its login", idsOnly, 42, "octo", true},
		{"a listed ID", idsOnly, 2002, "my-org", true},
		{"a listed ID after the account was renamed", idsOnly, 2002, "renamed-org", true},
		{"a listed login", loginsOnly, 2002, "my-org", true},
		{"a listed login, case-insensitively", loginsOnly, 2002, "MY-ORG", true},
		{"either list is enough: by ID", both, 2002, "renamed-org", true},
		{"either list is enough: by login", both, 3003, "my-org", true},
		{"a stranger", both, 9009, "stranger", false},
		// Why IDs are preferred: after my-org is renamed its old name is
		// free, and the login list accepts whoever registers it.
		{"another account that took a listed login over is accepted by the login list", loginsOnly, 9009, "my-org", true},
		{"another account that took the login over is refused when only IDs are listed", idsOnly, 9009, "my-org", false},
	} {
		if got := installationAccountFilter(tc.cfg)(tc.id, tc.login); got != tc.want {
			t.Errorf("%s: allowed(%d, %q) = %v, want %v", tc.name, tc.id, tc.login, got, tc.want)
		}
	}
}
