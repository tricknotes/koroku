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
		"public listen":       {"base_domain = \"example.com\"\nlisten = \"0.0.0.0:8080\"", "loopback"},
		"wildcard listen":     {"base_domain = \"example.com\"\nlisten = \":8080\"", "loopback"},
		"relative data dir":   {"base_domain = \"example.com\"\ndata_dir = \"data\"", "absolute path"},
		"bad public ip":       {"base_domain = \"example.com\"\npublic_ip = \"example.com\"", "not an IP address"},
		"unknown key":         {"base_domain = \"example.com\"\nbase_domian = \"x\"", "unknown keys: base_domian"},
		"bad disk limit":      {"base_domain = \"example.com\"\n[deploy]\ndisk_usage_limit_percent = 0", "disk_usage_limit_percent"},
		"bad port range":      {"base_domain = \"example.com\"\n[deploy]\nport_range_start = 30000", "port range"},
		"syntax error":        {`base_domain = `, "parse config"},
		"empty memory limit":  {"base_domain = \"example.com\"\n[deploy]\ndefault_memory_limit = \"\"", "default_memory_limit"},
		"bad memory limit":    {"base_domain = \"example.com\"\n[deploy]\ndefault_memory_limit = \"512 MB\"", "default_memory_limit"},
		"zero memory limit":   {"base_domain = \"example.com\"\n[deploy]\ndefault_memory_limit = \"0\"", "default_memory_limit"},
		"remote caddy admin":  {"base_domain = \"example.com\"\n[caddy]\nadmin_url = \"http://203.0.113.5:2019\"", "loopback"},
		"https caddy admin":   {"base_domain = \"example.com\"\n[caddy]\nadmin_url = \"https://127.0.0.1:2019\"", "admin_url"},
		"relative log dir":    {"base_domain = \"example.com\"\n[caddy]\naccess_log_dir = \"logs\"", "access_log_dir"},
		"plain http acme":     {"base_domain = \"example.com\"\n[caddy]\nacme_ca = \"http://ca.example.com/dir\"", "acme_ca"},
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

func TestCaddyDefaultsAndOptOut(t *testing.T) {
	cfg, err := Parse(`base_domain = "dev.example.com"`)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if !cfg.Caddy.Enabled || cfg.Caddy.AdminURL != "http://127.0.0.1:2019" || cfg.Caddy.AdminListen() != "127.0.0.1:2019" {
		t.Errorf("Caddy defaults = %+v", cfg.Caddy)
	}
	// With Caddy disabled its other settings are not validated.
	off, err := Parse("base_domain = \"dev.example.com\"\n[caddy]\nenabled = false\nadmin_url = \"\"")
	if err != nil || off.Caddy.Enabled {
		t.Errorf("disabled Caddy: %+v, %v", off.Caddy, err)
	}
	local, err := Parse("base_domain = \"dev.example.com\"\n[caddy]\nadmin_url = \"http://localhost:2019\"")
	if err != nil || local.Caddy.AdminListen() != "localhost:2019" {
		t.Errorf("localhost admin: %+v, %v", local.Caddy, err)
	}
}

func TestGitHubConfig(t *testing.T) {
	cfg, err := Parse(`base_domain = "dev.example.com"`)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if cfg.GitHub.Configured() {
		t.Error("GitHub must not count as configured by default")
	}
	if cfg.ConsoleURL != "https://console.dev.example.com" {
		t.Errorf("ConsoleURL = %q", cfg.ConsoleURL)
	}

	full := `base_domain = "dev.example.com"
console_url = "http://localhost:8080/"
[github]
app_id = 12345
client_id = "Iv1.example"
client_secret = "example-secret"
private_key_file = "/etc/paas/github-app.pem"
allowed_user_id = 42
`
	cfg, err = Parse(full)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if !cfg.GitHub.Configured() || cfg.GitHub.AllowedUserID != 42 || cfg.ConsoleURL != "http://localhost:8080" {
		t.Errorf("config = %+v / %q", cfg.GitHub, cfg.ConsoleURL)
	}

	// A partly filled section is rejected instead of half-working.
	for name, text := range map[string]string{
		"no allowed user": "[github]\napp_id = 1\nclient_id = \"a\"\nclient_secret = \"b\"",
		"no secret":       "[github]\napp_id = 1\nclient_id = \"a\"\nallowed_user_id = 42",
		"no app id":       "[github]\nclient_id = \"a\"\nclient_secret = \"b\"\nallowed_user_id = 42",
		"relative key":    "[github]\napp_id = 1\nclient_id = \"a\"\nclient_secret = \"b\"\nallowed_user_id = 42\nprivate_key_file = \"key.pem\"",
		"bad console url": "console_url = \"console.example.com\"",
		"console path":    "console_url = \"https://example.com/console\"",
	} {
		if _, err := Parse("base_domain = \"dev.example.com\"\n" + text); err == nil {
			t.Errorf("%s: expected an error", name)
		}
	}
	// Errors must not echo the client secret.
	_, err = Parse("base_domain = \"dev.example.com\"\n[github]\nclient_secret = \"super-secret-value\"")
	if err == nil || strings.Contains(err.Error(), "super-secret-value") {
		t.Errorf("err = %v", err)
	}
}

func TestStorageLogsAndBackupConfig(t *testing.T) {
	cfg, err := Parse(`base_domain = "dev.example.com"`)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if cfg.Storage.Configured() {
		t.Error("storage must not count as configured by default")
	}
	if cfg.Logs.Dir != "/var/lib/paas/logs" || cfg.Logs.RetentionDays != 14 || cfg.Logs.ArchiveIntervalHours != 4 {
		t.Errorf("Logs = %+v", cfg.Logs)
	}
	if cfg.Backup.Hour != 3 || cfg.Backup.ResticPasswordFile != "/etc/paas/restic.key" {
		t.Errorf("Backup = %+v", cfg.Backup)
	}
	if cfg.Timezone != "Asia/Tokyo" || cfg.Location().String() != "Asia/Tokyo" {
		t.Errorf("Timezone = %q / %v", cfg.Timezone, cfg.Location())
	}

	full := `base_domain = "dev.example.com"
data_dir = "/srv/paas"
timezone = "UTC"
[storage]
endpoint = "https://s3.example.com"
bucket = "paas"
access_key_id = "AKIAEXAMPLE"
secret_access_key = "example-secret-key"
[logs]
archive_interval_hours = 1
[backup]
hour = 4
`
	cfg, err = Parse(full)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if !cfg.Storage.Configured() || cfg.Storage.Region != "auto" || cfg.Logs.Dir != "/srv/paas/logs" || cfg.Backup.Hour != 4 {
		t.Errorf("config = %+v %+v %+v", cfg.Storage, cfg.Logs, cfg.Backup)
	}

	for name, text := range map[string]string{
		"no bucket":          "[storage]\nendpoint = \"https://s3.example.com\"\naccess_key_id = \"a\"\nsecret_access_key = \"b\"",
		"no credentials":     "[storage]\nendpoint = \"https://s3.example.com\"\nbucket = \"paas\"",
		"endpoint no scheme": "[storage]\nendpoint = \"s3.example.com\"\nbucket = \"paas\"\naccess_key_id = \"a\"\nsecret_access_key = \"b\"",
		"bad timezone":       "timezone = \"Mars/Olympus\"",
		"plain http storage": "[storage]\nendpoint = \"http://s3.example.com\"\nbucket = \"paas\"\naccess_key_id = \"a\"\nsecret_access_key = \"b\"",
		"zero retention":     "[logs]\nretention_days = 0",
		"archive too rare":   "[logs]\narchive_interval_hours = 48",
		"bad backup hour":    "[backup]\nhour = 24",
		"relative log dir":   "[logs]\ndir = \"logs\"",
	} {
		if _, err := Parse("base_domain = \"dev.example.com\"\n" + text); err == nil {
			t.Errorf("%s: expected an error", name)
		}
	}
	_, err = Parse("base_domain = \"dev.example.com\"\n[storage]\nsecret_access_key = \"super-secret-value\"")
	if err == nil || strings.Contains(err.Error(), "super-secret-value") {
		t.Errorf("err = %v", err)
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

func TestLoadRefusesReadableConfigWithSecrets(t *testing.T) {
	dir := t.TempDir()
	withSecret := "base_domain = \"dev.example.com\"\n[storage]\nendpoint = \"https://s3.example.com\"\nbucket = \"paas\"\naccess_key_id = \"a\"\nsecret_access_key = \"super-secret-value\"\n"
	path := filepath.Join(dir, "config.toml")
	if err := os.WriteFile(path, []byte(withSecret), 0o644); err != nil {
		t.Fatal(err)
	}
	_, err := Load(path)
	if err == nil || !strings.Contains(err.Error(), "chmod 600") {
		t.Fatalf("world-readable config with secrets: err = %v", err)
	}
	if strings.Contains(err.Error(), "super-secret-value") {
		t.Error("the error echoes the secret")
	}
	if err := os.Chmod(path, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(path); err != nil {
		t.Errorf("private config: %v", err)
	}
	// A config without credentials may be world-readable (development).
	plain := filepath.Join(dir, "plain.toml")
	if err := os.WriteFile(plain, []byte(`base_domain = "dev.example.com"`), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(plain); err != nil {
		t.Errorf("config without secrets: %v", err)
	}
}

func TestExampleConfigIsValid(t *testing.T) {
	if _, err := Load("../../deploy/config.example.toml"); err != nil {
		t.Fatalf("deploy/config.example.toml: %v", err)
	}
}
