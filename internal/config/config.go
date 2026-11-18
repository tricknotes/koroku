// Package config loads and validates the paasd configuration file.
package config

import (
	"errors"
	"fmt"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"
	_ "time/tzdata" // the binary must know Asia/Tokyo on a host without zoneinfo

	"github.com/BurntSushi/toml"
)

var memoryLimitPattern = regexp.MustCompile(`^[1-9][0-9]*[bkmg]?$`)

// DefaultPath is where paasd looks for its configuration when --config is not given.
const DefaultPath = "/etc/paas/config.toml"

// Config is the content of config.toml.
type Config struct {
	// BaseDomain is the domain under which apps get <slug>.<BaseDomain>.
	BaseDomain string `toml:"base_domain"`
	// ConsoleHost is the hostname of the management UI. Defaults to console.<BaseDomain>.
	ConsoleHost string `toml:"console_host"`
	// ConsoleURL is the public origin of the console. It defaults to
	// https://<ConsoleHost> and only needs to be set for local development
	// (for example http://localhost:8080).
	ConsoleURL string `toml:"console_url"`
	// Listen is the address paasd binds to. It must be a loopback address in production.
	Listen string `toml:"listen"`
	// DataDir holds the SQLite database, releases, env files and logs.
	DataDir string `toml:"data_dir"`
	// PublicIP is the address custom domains must resolve to.
	PublicIP string `toml:"public_ip"`
	// MasterKeyFile holds the key used to encrypt environment variable values.
	MasterKeyFile string `toml:"master_key_file"`

	// Timezone is the IANA zone used for schedules (daily backup, log
	// archive windows) and for the dates of log files.
	Timezone string `toml:"timezone"`

	Deploy  Deploy  `toml:"deploy"`
	Caddy   Caddy   `toml:"caddy"`
	GitHub  GitHub  `toml:"github"`
	Storage Storage `toml:"storage"`
	Logs    Logs    `toml:"logs"`
	Backup  Backup  `toml:"backup"`
}

// Storage is the S3-compatible object storage used for log archives and
// restic backups (Cloudflare R2 in production; nothing R2-specific is used).
type Storage struct {
	Endpoint        string `toml:"endpoint"`
	Region          string `toml:"region"`
	Bucket          string `toml:"bucket"`
	AccessKeyID     string `toml:"access_key_id"`
	SecretAccessKey string `toml:"secret_access_key"`
}

// Configured reports whether object storage is set up at all.
func (s Storage) Configured() bool {
	return s.Endpoint != "" || s.Bucket != "" || s.AccessKeyID != "" || s.SecretAccessKey != ""
}

// Logs holds log collection settings.
type Logs struct {
	// Dir holds the collected logs. Defaults to <data_dir>/logs.
	Dir string `toml:"dir"`
	// RetentionDays is how long logs stay on local disk.
	RetentionDays int `toml:"retention_days"`
	// ArchiveIntervalHours is how often logs are archived to object storage.
	ArchiveIntervalHours int `toml:"archive_interval_hours"`
}

// Backup holds backup settings. Backups need [storage] as well.
type Backup struct {
	// ResticPasswordFile holds the restic repository password (mode 0600).
	ResticPasswordFile string `toml:"restic_password_file"`
	// Hour is the local hour (in Timezone) of the daily backup.
	Hour int `toml:"hour"`
}

// GitHub holds the GitHub App used for login and repository access.
type GitHub struct {
	AppID        int64  `toml:"app_id"`
	ClientID     string `toml:"client_id"`
	ClientSecret string `toml:"client_secret"`
	// PrivateKeyFile is the App's private key in PEM format (mode 0600).
	PrivateKeyFile string `toml:"private_key_file"`
	// AllowedUserID is the numeric ID of the only GitHub user who may log in.
	AllowedUserID int64 `toml:"allowed_user_id"`
}

// Configured reports whether a GitHub App is set up at all.
func (g GitHub) Configured() bool {
	return g.AppID != 0 || g.ClientID != "" || g.ClientSecret != "" || g.AllowedUserID != 0
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
		GitHub:        GitHub{PrivateKeyFile: "/etc/paas/github-app.pem"},
		Timezone:      "Asia/Tokyo",
		Storage:       Storage{Region: "auto"},
		Logs:          Logs{RetentionDays: 14, ArchiveIntervalHours: 4},
		Backup:        Backup{ResticPasswordFile: "/etc/paas/restic.key", Hour: 3},
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
	if cfg.Logs.Dir == "" {
		cfg.Logs.Dir = filepath.Join(cfg.DataDir, "logs")
	}
	if cfg.ConsoleURL == "" && cfg.ConsoleHost != "" {
		cfg.ConsoleURL = "https://" + cfg.ConsoleHost
	}
	cfg.ConsoleURL = strings.TrimRight(cfg.ConsoleURL, "/")
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
	if u, err := url.Parse(c.ConsoleURL); c.ConsoleURL != "" &&
		(err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Host == "" || u.Path != "" || u.RawQuery != "") {
		errs = append(errs, fmt.Errorf("console_url %q must be an origin such as https://console.example.com", c.ConsoleURL))
	}
	if _, err := time.LoadLocation(c.Timezone); err != nil || c.Timezone == "" {
		errs = append(errs, fmt.Errorf("timezone %q is not a known time zone", c.Timezone))
	}
	if st := c.Storage; st.Configured() {
		// Half-configured storage would silently skip backups.
		if u, err := url.Parse(st.Endpoint); err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Host == "" {
			errs = append(errs, fmt.Errorf("storage.endpoint %q must be a URL such as https://s3.example.com", st.Endpoint))
		}
		if st.Region == "" {
			errs = append(errs, errors.New("storage.region is required"))
		}
		if st.Bucket == "" {
			errs = append(errs, errors.New("storage.bucket is required"))
		}
		if st.AccessKeyID == "" || st.SecretAccessKey == "" {
			errs = append(errs, errors.New("storage.access_key_id and storage.secret_access_key are required"))
		}
	}
	if !filepath.IsAbs(c.Logs.Dir) {
		errs = append(errs, fmt.Errorf("logs.dir %q must be an absolute path", c.Logs.Dir))
	}
	if c.Logs.RetentionDays < 1 {
		errs = append(errs, fmt.Errorf("logs.retention_days %d must be at least 1", c.Logs.RetentionDays))
	}
	if h := c.Logs.ArchiveIntervalHours; h < 1 || h > 24 {
		errs = append(errs, fmt.Errorf("logs.archive_interval_hours %d must be between 1 and 24", h))
	}
	if !filepath.IsAbs(c.Backup.ResticPasswordFile) {
		errs = append(errs, fmt.Errorf("backup.restic_password_file %q must be an absolute path", c.Backup.ResticPasswordFile))
	}
	if c.Backup.Hour < 0 || c.Backup.Hour > 23 {
		errs = append(errs, fmt.Errorf("backup.hour %d must be between 0 and 23", c.Backup.Hour))
	}
	if g := c.GitHub; g.Configured() {
		// A half-configured App would lock the console or, worse, look
		// configured while accepting nobody. Require everything.
		if g.AppID <= 0 {
			errs = append(errs, errors.New("github.app_id is required"))
		}
		if g.ClientID == "" {
			errs = append(errs, errors.New("github.client_id is required"))
		}
		if g.ClientSecret == "" {
			errs = append(errs, errors.New("github.client_secret is required"))
		}
		if !filepath.IsAbs(g.PrivateKeyFile) {
			errs = append(errs, fmt.Errorf("github.private_key_file %q must be an absolute path", g.PrivateKeyFile))
		}
		if g.AllowedUserID <= 0 {
			errs = append(errs, errors.New("github.allowed_user_id is required"))
		}
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
	if !memoryLimitPattern.MatchString(c.Deploy.DefaultMemoryLimit) {
		// A malformed value would make every deployment fail at Compose time.
		errs = append(errs, fmt.Errorf("deploy.default_memory_limit %q must be a number with an optional unit (b, k, m, g), for example 512m", c.Deploy.DefaultMemoryLimit))
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

// Location returns the configured time zone. Validate guarantees it loads.
func (c Config) Location() *time.Location {
	loc, err := time.LoadLocation(c.Timezone)
	if err != nil {
		return time.UTC
	}
	return loc
}

// DBPath is the location of the SQLite database.
func (c Config) DBPath() string { return filepath.Join(c.DataDir, "paas.db") }

// AppsDir is the directory holding per-app releases, env files and deploy logs.
func (c Config) AppsDir() string { return filepath.Join(c.DataDir, "apps") }
