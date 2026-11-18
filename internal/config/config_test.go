package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestParseAppliesDefaults(t *testing.T) {
	cfg, err := Parse(`base_domain = "dev.example.com"`)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if cfg.ConsoleHost != "console.dev.example.com" {
		t.Errorf("ConsoleHost = %q", cfg.ConsoleHost)
	}
	if cfg.Listen != "127.0.0.1:8080" {
		t.Errorf("Listen = %q", cfg.Listen)
	}
	if cfg.DBPath() != "/var/lib/paas/paas.db" {
		t.Errorf("DBPath = %q", cfg.DBPath())
	}
	if cfg.Deploy.DiskUsageLimitPercent != 85 {
		t.Errorf("DiskUsageLimitPercent = %d", cfg.Deploy.DiskUsageLimitPercent)
	}
	if cfg.Deploy.PortRangeStart != 20000 {
		t.Errorf("PortRangeStart = %d", cfg.Deploy.PortRangeStart)
	}
}

func TestParseOverrides(t *testing.T) {
	cfg, err := Parse(`
base_domain = "dev.example.com"
console_host = "admin.example.com"
listen = "127.0.0.1:9000"
data_dir = "/srv/paas"
public_ip = "203.0.113.10"

[deploy]
default_memory_limit = "1g"
disk_usage_limit_percent = 70
`)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if cfg.ConsoleHost != "admin.example.com" || cfg.Listen != "127.0.0.1:9000" || cfg.DataDir != "/srv/paas" {
		t.Errorf("unexpected config: %+v", cfg)
	}
	if cfg.Deploy.DefaultMemoryLimit != "1g" || cfg.Deploy.DiskUsageLimitPercent != 70 {
		t.Errorf("unexpected deploy config: %+v", cfg.Deploy)
	}
	// Unset keys in a section keep their defaults.
	if cfg.Deploy.PortRangeEnd != 20999 {
		t.Errorf("PortRangeEnd = %d", cfg.Deploy.PortRangeEnd)
	}
}

func TestParseRejectsInvalid(t *testing.T) {
	cases := map[string]struct {
		text string
		want string
	}{
		"missing base domain": {``, "base_domain is required"},
		"bad base domain":     {`base_domain = "https://example.com"`, "is not a hostname"},
		"bad listen":          {"base_domain = \"example.com\"\nlisten = \"nope\"", "listen"},
		"relative data dir":   {"base_domain = \"example.com\"\ndata_dir = \"data\"", "absolute path"},
		"bad public ip":       {"base_domain = \"example.com\"\npublic_ip = \"example.com\"", "not an IP address"},
		"unknown key":         {"base_domain = \"example.com\"\nbase_domian = \"x\"", "unknown keys: base_domian"},
		"bad disk limit":      {"base_domain = \"example.com\"\n[deploy]\ndisk_usage_limit_percent = 0", "disk_usage_limit_percent"},
		"bad port range":      {"base_domain = \"example.com\"\n[deploy]\nport_range_start = 30000", "port range"},
		"syntax error":        {`base_domain = `, "parse config"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			_, err := Parse(tc.text)
			if err == nil {
				t.Fatal("expected an error")
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error %q does not contain %q", err, tc.want)
			}
		})
	}
}

func TestLoad(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.toml")
	if err := os.WriteFile(path, []byte(`base_domain = "dev.example.com"`), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.BaseDomain != "dev.example.com" {
		t.Errorf("BaseDomain = %q", cfg.BaseDomain)
	}
	if _, err := Load(filepath.Join(t.TempDir(), "missing.toml")); err == nil {
		t.Error("expected an error for a missing file")
	}
}

func TestExampleConfigIsValid(t *testing.T) {
	if _, err := Load("../../deploy/config.example.toml"); err != nil {
		t.Fatalf("deploy/config.example.toml: %v", err)
	}
}
