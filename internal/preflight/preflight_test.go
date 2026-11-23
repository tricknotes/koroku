package preflight

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/tricknotes/koroku/internal/backup"
	"github.com/tricknotes/koroku/internal/config"
	"github.com/tricknotes/koroku/internal/github"
	"github.com/tricknotes/koroku/internal/objstore"
)

const testIP = "198.51.100.7"

// resolver answers from a fixed table; "*.<domain>" entries act as wildcards.
type resolver map[string]string

func (r resolver) LookupIPAddr(_ context.Context, host string) ([]net.IPAddr, error) {
	host = strings.TrimSuffix(host, ".")
	ip, found := r[host]
	if !found {
		if i := strings.IndexByte(host, '.'); i > 0 {
			ip, found = r["*"+host[i:]]
		}
	}
	if !found {
		return nil, &net.DNSError{Err: "no such host", Name: host, IsNotFound: true}
	}
	return []net.IPAddr{{IP: net.ParseIP(ip)}}, nil
}

type fakeApp struct {
	app      github.App
	appErr   error
	installs []github.Installation
	hook     github.WebhookConfig
	hookErr  error
}

func (f fakeApp) App(context.Context) (github.App, error) { return f.app, f.appErr }
func (f fakeApp) WebhookConfig(context.Context) (github.WebhookConfig, error) {
	return f.hook, f.hookErr
}
func (f fakeApp) Installations(context.Context) ([]github.Installation, error) {
	return f.installs, nil
}

type fakeUsers map[int64]string

func (f fakeUsers) UserByID(_ context.Context, id int64) (github.User, error) {
	login, found := f[id]
	if !found {
		return github.User{}, github.ErrNotFound
	}
	return github.User{ID: id, Login: login}, nil
}

type fakeBackups struct {
	snaps []backup.Snapshot
	err   error
}

func (f fakeBackups) SystemSnapshots(context.Context) ([]backup.Snapshot, error) {
	return f.snaps, f.err
}

var now = time.Date(2026, 11, 20, 12, 0, 0, 0, time.UTC)

// healthy returns dependencies for which every check passes.
func healthy(t *testing.T) Deps {
	t.Helper()
	dir := t.TempDir()
	keyFile := filepath.Join(dir, "master.key")
	if err := os.WriteFile(keyFile, []byte("k"), 0o600); err != nil {
		t.Fatal(err)
	}
	caddy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/config/" {
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(caddy.Close)

	cfg := config.Default()
	cfg.BaseDomain = "apps.test"
	cfg.ConsoleHost = "console.apps.test"
	// The console check needs https; it is pointed at a test server per test.
	cfg.ConsoleURL = "https://console.apps.test"
	cfg.PublicIP = testIP
	cfg.MasterKeyFile = keyFile
	cfg.Caddy.AdminURL = caddy.URL
	cfg.GitHub = config.GitHub{AppID: 42, ClientID: "Iv1.abc", ClientSecret: "s", PrivateKeyFile: "/k", AllowedUserID: 7,
		WebhookSecret: "0123456789abcdef0123"}
	cfg.Storage = config.Storage{Endpoint: "https://s3.test", Region: "auto", Bucket: "koroku", AccessKeyID: "a", SecretAccessKey: "b"}

	console := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
	}))
	t.Cleanup(console.Close)
	client := console.Client()
	// Send the console host to the TLS test server, and anything else as usual.
	transport := client.Transport.(*http.Transport).Clone()
	consoleAddr := console.Listener.Addr().String()
	dialer := &net.Dialer{}
	transport.DialContext = func(ctx context.Context, network, addr string) (net.Conn, error) {
		if addr == "console.apps.test:443" {
			addr = consoleAddr
		}
		return dialer.DialContext(ctx, network, addr)
	}
	transport.TLSClientConfig.ServerName = "example.com" // the name in httptest's certificate
	client.Transport = transport

	return Deps{
		Config:   cfg,
		Resolver: resolver{"console.apps.test": testIP, "*.apps.test": testIP},
		HostAddrs: func() ([]net.Addr, error) {
			return []net.Addr{&net.IPNet{IP: net.ParseIP(testIP), Mask: net.CIDRMask(24, 32)}}, nil
		},
		HTTP: client,
		GitHub: fakeApp{
			app: github.App{ID: 42, Slug: "koroku-test", ClientID: "Iv1.abc", HTMLURL: "https://github.com/apps/koroku-test",
				Permissions: map[string]string{"contents": "read", "metadata": "read", "deployments": "write", "actions": "read", "pull_requests": "read"},
				Events:      []string{"push", "workflow_run"}},
			hook:     github.WebhookConfig{URL: "https://console.apps.test/webhooks/github", ContentType: "json"},
			installs: []github.Installation{{ID: 1, Account: "octocat", AccountID: 7, RepositorySelection: "all", Allowed: true}},
		},
		Users:          fakeUsers{7: "octocat"},
		AutoDeployApps: []string{"blog", "shop"},
		WorkflowApps:   []string{"shop"},
		Objects:        objstore.NewMemory(),
		Backups:        fakeBackups{snaps: []backup.Snapshot{{ID: "abcd1234", Time: now.Add(-3 * time.Hour)}}},
		Now:            func() time.Time { return now },
	}
}

func byName(results []Result) map[string]Result {
	m := make(map[string]Result, len(results))
	for _, r := range results {
		m[r.Name] = r
	}
	return m
}

func TestHealthyHost(t *testing.T) {
	results := Run(context.Background(), healthy(t))
	for _, r := range results {
		if r.Status != OK {
			t.Errorf("%s: %s %q (hint %q)", r.Name, r.Status, r.Message, r.Hint)
		}
	}
	want := []string{"config", "public_ip", "dns.console", "dns.apps", "caddy", "https", "master_key",
		"github.app", "github.install", "github.user", "github.webhook", "github.workflow", "storage", "backup"}
	if len(results) != len(want) {
		t.Fatalf("got %d results, want %d", len(results), len(want))
	}
	for i, name := range want {
		if results[i].Name != name {
			t.Errorf("result %d is %q, want %q", i, results[i].Name, name)
		}
	}
}

// The installation check prints each account's ID, so that the operator can
// list the accounts by ID without looking the IDs up elsewhere.
func TestInstallationCheck(t *testing.T) {
	owner := github.Installation{ID: 1, Account: "octocat", AccountID: 7, RepositorySelection: "selected", Allowed: true}
	org := github.Installation{ID: 2, Account: "my-org", AccountID: 2002, RepositorySelection: "all", Allowed: true}
	other := github.Installation{ID: 3, Account: "other-org", AccountID: 3003, RepositorySelection: "all", Allowed: true}
	stranger := github.Installation{ID: 4, Account: "stranger", AccountID: 9009, RepositorySelection: "all"}
	const used = "octocat (ID 7, selected repositories), my-org (ID 2002, all repositories)"
	const byLogin = "; my-org matched by login only (installation_accounts), " +
		"and a login given up in a rename can be taken by another account"
	for name, tc := range map[string]struct {
		ids      []int64
		logins   []string
		installs []github.Installation
		want     Status
		message  string
		hint     string
	}{
		"listed by ID": {
			ids:      []int64{2002},
			installs: []github.Installation{owner, org},
			want:     OK,
			message:  "installed on " + used,
		},
		"matched by login only": {
			logins:   []string{"My-Org"},
			installs: []github.Installation{owner, org},
			want:     Warn,
			message:  "installed on " + used + byLogin,
			hint:     "write installation_account_ids = [2002] in [github] and remove installation_accounts (docs/SETUP.md 4)",
		},
		"one by ID, one by login: the line keeps the configured IDs": {
			ids:      []int64{3003},
			logins:   []string{"my-org"},
			installs: []github.Installation{owner, org, other},
			want:     Warn,
			message:  "installed on " + used + ", other-org (ID 3003, all repositories)" + byLogin,
			hint:     "write installation_account_ids = [3003, 2002] in [github] and remove installation_accounts (docs/SETUP.md 4)",
		},
		"a login left next to its ID": {
			ids:      []int64{2002},
			logins:   []string{"my-org"},
			installs: []github.Installation{owner, org},
			want:     Warn,
			message: "installed on " + used + "; installation_accounts lists logins that no used installation needs, " +
				"and a listed login can be taken by another account",
			hint: "remove installation_accounts from [github] (docs/SETUP.md 4)",
		},
		"an account koroku ignores": {
			ids:      []int64{2002},
			installs: []github.Installation{owner, org, stranger},
			want:     Warn,
			message:  "installed on stranger (ID 9009), which koroku ignores; used: " + used,
			hint: "add the ID of an account you manage to [github] installation_account_ids; " +
				"uninstall the App from accounts you do not know (docs/SETUP.md 4)",
		},
		"ignored and matched by login": {
			logins:   []string{"my-org"},
			installs: []github.Installation{owner, org, stranger},
			want:     Warn,
			message:  "installed on stranger (ID 9009), which koroku ignores; used: " + used + byLogin,
			hint: "add the ID of an account you manage to [github] installation_account_ids; " +
				"uninstall the App from accounts you do not know; " +
				"write installation_account_ids = [2002] in [github] and remove installation_accounts (docs/SETUP.md 4)",
		},
	} {
		t.Run(name, func(t *testing.T) {
			d := healthy(t)
			d.Config.GitHub.InstallationAccountIDs = tc.ids
			d.Config.GitHub.InstallationAccounts = tc.logins
			a := d.GitHub.(fakeApp)
			a.installs = tc.installs
			d.GitHub = a
			got := byName(Run(context.Background(), d))["github.install"]
			if got.Status != tc.want || got.Message != tc.message || got.Hint != tc.hint {
				t.Errorf("github.install = %s\n message %q\n    want %q\n    hint %q\n    want %q",
					got.Status, got.Message, tc.message, got.Hint, tc.hint)
			}
		})
	}
}

func TestStorageCheckCleansUp(t *testing.T) {
	d := healthy(t)
	mem := objstore.NewMemory()
	d.Objects = mem
	Run(context.Background(), d)
	left, err := mem.List(context.Background(), "")
	if err != nil {
		t.Fatal(err)
	}
	if len(left) != 0 {
		t.Errorf("objects left in the bucket: %v", left)
	}
}

func TestProblems(t *testing.T) {
	cases := map[string]struct {
		change func(*Deps)
		check  string
		want   Status
		text   string
	}{
		"example values": {
			change: func(d *Deps) { d.Config.PublicIP = "203.0.113.10" },
			check:  "config", want: Fail, text: "public_ip",
		},
		"no public ip": {
			change: func(d *Deps) { d.Config.PublicIP = "" },
			check:  "public_ip", want: Fail,
		},
		"behind NAT": {
			change: func(d *Deps) { d.HostAddrs = func() ([]net.Addr, error) { return nil, nil } },
			check:  "public_ip", want: Warn,
		},
		"console points elsewhere": {
			change: func(d *Deps) { d.Resolver = resolver{"console.apps.test": "192.0.2.1", "*.apps.test": testIP} },
			check:  "dns.console", want: Fail,
		},
		"no wildcard": {
			change: func(d *Deps) { d.Resolver = resolver{"console.apps.test": testIP} },
			check:  "dns.apps", want: Warn, text: "wildcard",
		},
		"caddy down": {
			change: func(d *Deps) { d.Config.Caddy.AdminURL = "http://127.0.0.1:1" },
			check:  "caddy", want: Fail,
		},
		"untrusted certificate": {
			change: func(d *Deps) {
				d.HTTP = &http.Client{Transport: d.HTTP.Transport.(*http.Transport).Clone()}
				untrust(d)
			},
			check: "https", want: Fail,
		},
		"staging certificate": {
			change: func(d *Deps) {
				d.Config.Caddy.ACMECA = "https://acme-staging-v02.api.letsencrypt.org/directory"
				untrust(d)
			},
			check: "https", want: Warn, text: "staging",
		},
		"no master key yet": {
			change: func(d *Deps) { d.Config.MasterKeyFile = filepath.Join(t.TempDir(), "missing") },
			check:  "master_key", want: Warn,
		},
		"no github": {
			change: func(d *Deps) { d.Config.GitHub = config.GitHub{} },
			check:  "github.app", want: Fail, text: "nobody can log in",
		},
		"key of another app": {
			change: func(d *Deps) { d.GitHub = fakeApp{appErr: github.ErrUnauthorized} },
			check:  "github.app", want: Fail, text: "rejected",
		},
		"missing contents permission": {
			change: func(d *Deps) {
				a := d.GitHub.(fakeApp)
				a.app.Permissions = map[string]string{"metadata": "read"}
				d.GitHub = a
			},
			check: "github.app", want: Fail, text: "contents: read",
		},
		"write permission": {
			change: func(d *Deps) {
				a := d.GitHub.(fakeApp)
				a.app.Permissions = map[string]string{"contents": "read", "metadata": "read", "deployments": "write", "issues": "write"}
				d.GitHub = a
			},
			check: "github.app", want: Warn, text: "issues: write",
		},
		"no deployments permission": {
			change: func(d *Deps) {
				a := d.GitHub.(fakeApp)
				a.app.Permissions = map[string]string{"contents": "read", "metadata": "read"}
				d.GitHub = a
			},
			check: "github.app", want: Warn, text: "GitHub deployments will not be updated",
		},
		"no pull requests permission": {
			change: func(d *Deps) {
				a := d.GitHub.(fakeApp)
				a.app.Permissions = map[string]string{"contents": "read", "metadata": "read", "deployments": "write"}
				d.GitHub = a
			},
			check: "github.app", want: Warn, text: "review apps cannot be created",
		},
		"read-only deployments permission": {
			change: func(d *Deps) {
				a := d.GitHub.(fakeApp)
				a.app.Permissions = map[string]string{"contents": "read", "metadata": "read", "deployments": "read"}
				d.GitHub = a
			},
			check: "github.app", want: Warn, text: "deployments: read",
		},
		"missing contents with deployments": {
			change: func(d *Deps) {
				a := d.GitHub.(fakeApp)
				a.app.Permissions = map[string]string{"metadata": "read", "deployments": "write"}
				d.GitHub = a
			},
			check: "github.app", want: Fail, text: "contents: read",
		},
		"not installed": {
			change: func(d *Deps) {
				a := d.GitHub.(fakeApp)
				a.installs = nil
				d.GitHub = a
			},
			check: "github.install", want: Warn,
		},
		"installed on an account koroku ignores": {
			change: func(d *Deps) {
				a := d.GitHub.(fakeApp)
				a.installs = append(a.installs, github.Installation{ID: 2, Account: "stranger", AccountID: 9009, RepositorySelection: "all"})
				d.GitHub = a
			},
			check: "github.install", want: Warn, text: "stranger (ID 9009), which koroku ignores",
		},
		"unknown user": {
			change: func(d *Deps) { d.Users = fakeUsers{} },
			check:  "github.user", want: Fail,
		},
		"no storage": {
			change: func(d *Deps) { d.Config.Storage = config.Storage{}; d.Objects = nil },
			check:  "storage", want: Fail,
		},
		"storage refuses writes": {
			change: func(d *Deps) { d.Objects = refusingStore{d.Objects} },
			check:  "storage", want: Fail, text: "write",
		},
		"no restic password": {
			change: func(d *Deps) { d.Backups = nil; d.BackupsErr = errors.New("the restic password file is missing") },
			check:  "backup", want: Fail, text: "password",
		},
		"wrong restic password": {
			change: func(d *Deps) { d.Backups = fakeBackups{err: errors.New("exit status 12")} },
			check:  "backup", want: Fail,
		},
		"no snapshot yet": {
			change: func(d *Deps) { d.Backups = fakeBackups{} },
			check:  "backup", want: Warn,
		},
		"old snapshot": {
			change: func(d *Deps) {
				d.Backups = fakeBackups{snaps: []backup.Snapshot{{ID: "x", Time: now.Add(-50 * time.Hour)}}}
			},
			check: "backup", want: Warn,
		},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			d := healthy(t)
			tc.change(&d)
			r := byName(Run(context.Background(), d))[tc.check]
			if r.Status != tc.want {
				t.Fatalf("%s: status %s (%q), want %s", tc.check, r.Status, r.Message, tc.want)
			}
			if tc.text != "" && !strings.Contains(r.Message, tc.text) {
				t.Errorf("message %q does not mention %q", r.Message, tc.text)
			}
			if r.Hint == "" {
				t.Errorf("no hint for %s", tc.check)
			}
		})
	}
}

// untrust makes the client reject the test server's certificate.
func untrust(d *Deps) {
	tr := d.HTTP.Transport.(*http.Transport).Clone()
	tr.TLSClientConfig.RootCAs = nil
	d.HTTP = &http.Client{Transport: tr}
}

type refusingStore struct{ objstore.Store }

func (refusingStore) Put(context.Context, string, []byte, string) error {
	return errors.New("403 AccessDenied")
}

func TestHTTPSFailureTellsCertificatesApart(t *testing.T) {
	unknown := func(cn string, org ...string) error {
		cert := &x509.Certificate{Issuer: pkix.Name{CommonName: cn, Organization: org}}
		return &url.Error{Op: "Get", URL: "https://console.example.com/api/system",
			Err: &tls.CertificateVerificationError{Err: x509.UnknownAuthorityError{Cert: cert}}}
	}
	cases := map[string]struct {
		err     error
		staging bool
		want    Status
		text    string
		hint    string
	}{
		"staging while configured":      {unknown("(STAGING) Pretend Pear X1"), true, Warn, "staging certificate", "delete the acme_ca line"},
		"staging after switching away":  {unknown("(STAGING) Pretend Pear X1"), false, Fail, "still serves", "systemctl restart caddy"},
		"staging named by organization": {unknown("", "(STAGING) Let's Encrypt"), false, Fail, "still serves", "restart caddy"},
		"some other authority":          {unknown("Example Private CA"), false, Fail, "Example Private CA", "journalctl -u caddy"},
		"no answer at all":              {errors.New("dial tcp: connection refused"), false, Fail, "connection refused", "80/443"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			r := httpsFailure(tc.err, "https://console.example.com", "console.example.com", tc.staging)
			if r.Status != tc.want || !strings.Contains(r.Message, tc.text) || !strings.Contains(r.Hint, tc.hint) {
				t.Errorf("got %s %q (hint %q)", r.Status, r.Message, r.Hint)
			}
		})
	}
}
