// Package config loads and validates the paasd configuration file.
package config

import (
	"errors"
	"fmt"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"strings"

	"github.com/BurntSushi/toml"
)

// DefaultPath is where paasd looks for its configuration when --config is not given.
const DefaultPath = "/etc/paas/config.toml"

// Config is the content of config.toml.
type Config struct {
	// BaseDomain is the domain under which apps get <slug>.<BaseDomain>.
	BaseDomain string `toml:"base_domain"`
	// ConsoleHost is the hostname of the management UI. Defaults to console.<BaseDomain>.
	ConsoleHost string `toml:"console_host"`
	// Listen is the address paasd binds to. It must be a loopback address in production.
	Listen string `toml:"listen"`
	// DataDir holds the SQLite database, releases, env files and logs.
	DataDir string `toml:"data_dir"`
	// PublicIP is the address custom domains must resolve to.
	PublicIP string `toml:"public_ip"`
	// MasterKeyFile holds the key used to encrypt environment variable values.
	MasterKeyFile string `toml:"master_key_file"`

	Deploy Deploy `toml:"deploy"`
	Caddy  Caddy  `toml:"caddy"`
}

// Caddy holds reverse proxy settings.
type Caddy struct {
	// Enabled makes paasd load its generated configuration into Caddy.
	// Turn it off for local development without Caddy.
	Enabled bool `toml:"enabled"`
	// AdminURL is the Caddy admin API. It must be a loopback address.
	AdminURL string `toml:"admin_url"`
	// ACMECA overrides the ACME directory, for example with the Let's
	// Encrypt staging endpoint while the setup is being tested.
	ACMECA string `toml:"acme_ca"`
	// ACMEEmail is the optional contact address of the ACME account.
	ACMEEmail string `toml:"acme_email"`
	// AccessLogDir receives one access log per app (<slug>.log). Caddy
	// writes there, so its user needs write access. Empty disables them.
	AccessLogDir string `toml:"access_log_dir"`
}

// Deploy holds deploy engine settings.
type Deploy struct {
	// DefaultMemoryLimit is applied to every service unless the app overrides it.
	DefaultMemoryLimit string `toml:"default_memory_limit"`
	// DiskUsageLimitPercent rejects new deployments when the data disk is fuller than this.
	DiskUsageLimitPercent int `toml:"disk_usage_limit_percent"`
	// PortRangeStart and PortRangeEnd bound the host ports assigned to apps.
	PortRangeStart int `toml:"port_range_start"`
	PortRangeEnd   int `toml:"port_range_end"`
	// PruneImages runs `docker image prune -f` after each successful deploy.
	PruneImages bool `toml:"prune_images"`
	// AllowFileRepos accepts file:// repository URLs. Only for local
	// development and tests; keep it off on a real host.
	AllowFileRepos bool `toml:"allow_file_repos"`
}

// Default returns the configuration used for fields that are not set in the file.
func Default() Config {
	return Config{
		Listen:        "127.0.0.1:8080",
		DataDir:       "/var/lib/paas",
		MasterKeyFile: "/etc/paas/master.key",
		Caddy: Caddy{
			Enabled:      true,
			AdminURL:     "http://127.0.0.1:2019",
			AccessLogDir: "/var/log/caddy/apps",
		},
		Deploy: Deploy{
			DefaultMemoryLimit:    "512m",
			DiskUsageLimitPercent: 85,
			PortRangeStart:        20000,
			PortRangeEnd:          20999,
			PruneImages:           true,
		},
	}
}

// Load reads the TOML file at path, applies defaults and validates the result.
func Load(path string) (Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return Config{}, fmt.Errorf("read config: %w", err)
	}
	return Parse(string(data))
}

// Parse decodes TOML text, applies defaults and validates the result.
func Parse(text string) (Config, error) {
	cfg := Default()
	meta, err := toml.Decode(text, &cfg)
	if err != nil {
		return Config{}, fmt.Errorf("parse config: %w", err)
	}
	if undecoded := meta.Undecoded(); len(undecoded) > 0 {
		keys := make([]string, len(undecoded))
		for i, k := range undecoded {
			keys[i] = k.String()
		}
		return Config{}, fmt.Errorf("parse config: unknown keys: %s", strings.Join(keys, ", "))
	}
	if cfg.ConsoleHost == "" && cfg.BaseDomain != "" {
		cfg.ConsoleHost = "console." + cfg.BaseDomain
	}
	if err := cfg.Validate(); err != nil {
		return Config{}, fmt.Errorf("invalid config: %w", err)
	}
	return cfg, nil
}

// Validate reports every problem found in the configuration at once.
func (c Config) Validate() error {
	var errs []error
	if c.BaseDomain == "" {
		errs = append(errs, errors.New("base_domain is required"))
	} else if strings.ContainsAny(c.BaseDomain, "/: ") || strings.HasPrefix(c.BaseDomain, ".") {
		errs = append(errs, fmt.Errorf("base_domain %q is not a hostname", c.BaseDomain))
	}
	if _, _, err := net.SplitHostPort(c.Listen); err != nil {
		errs = append(errs, fmt.Errorf("listen %q: %w", c.Listen, err))
	}
	if !filepath.IsAbs(c.DataDir) {
		errs = append(errs, fmt.Errorf("data_dir %q must be an absolute path", c.DataDir))
	}
	if c.PublicIP != "" && net.ParseIP(c.PublicIP) == nil {
		errs = append(errs, fmt.Errorf("public_ip %q is not an IP address", c.PublicIP))
	}
	if p := c.Deploy.DiskUsageLimitPercent; p < 1 || p > 100 {
		errs = append(errs, fmt.Errorf("deploy.disk_usage_limit_percent %d must be between 1 and 100", p))
	}
	if s, e := c.Deploy.PortRangeStart, c.Deploy.PortRangeEnd; s < 1024 || e > 65535 || s > e {
		errs = append(errs, fmt.Errorf("deploy port range %d-%d is invalid", s, e))
	}
	if c.Caddy.Enabled {
		if host, err := adminHost(c.Caddy.AdminURL); err != nil {
			errs = append(errs, fmt.Errorf("caddy.admin_url %q: %w", c.Caddy.AdminURL, err))
		} else if ip := net.ParseIP(host); host != "localhost" && (ip == nil || !ip.IsLoopback()) {
			// The admin API is unauthenticated and can rewrite the whole proxy.
			errs = append(errs, fmt.Errorf("caddy.admin_url %q must be a loopback address", c.Caddy.AdminURL))
		}
		if c.Caddy.AccessLogDir != "" && !filepath.IsAbs(c.Caddy.AccessLogDir) {
			errs = append(errs, fmt.Errorf("caddy.access_log_dir %q must be an absolute path", c.Caddy.AccessLogDir))
		}
		if c.Caddy.ACMECA != "" && !strings.HasPrefix(c.Caddy.ACMECA, "https://") {
			errs = append(errs, fmt.Errorf("caddy.acme_ca %q must be an https URL", c.Caddy.ACMECA))
		}
	}
	if c.Deploy.DefaultMemoryLimit == "" {
		errs = append(errs, errors.New("deploy.default_memory_limit is required"))
	}
	return errors.Join(errs...)
}

func adminHost(raw string) (string, error) {
	u, err := url.Parse(raw)
	if err != nil {
		return "", errors.New("not a valid URL")
	}
	if u.Scheme != "http" || u.Hostname() == "" {
		return "", errors.New("want http://host:port")
	}
	return u.Hostname(), nil
}

// AdminListen is the host:port form of the Caddy admin address.
func (c Caddy) AdminListen() string {
	u, err := url.Parse(c.AdminURL)
	if err != nil {
		return ""
	}
	return u.Host
}

// DBPath is the location of the SQLite database.
func (c Config) DBPath() string { return filepath.Join(c.DataDir, "paas.db") }

// AppsDir is the directory holding per-app releases, env files and deploy logs.
func (c Config) AppsDir() string { return filepath.Join(c.DataDir, "apps") }
