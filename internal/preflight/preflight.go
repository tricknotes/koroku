// Package preflight checks that a host and the external services koroku
// depends on (DNS, Caddy and its certificates, the GitHub App, object storage
// and restic) are set up as docs/SETUP.md describes. `koroku check` runs it
// after provisioning and whenever a setting changes.
//
// Every check reports a status, a one-line message and, when something is
// wrong, a hint that names the section of docs/SETUP.md to read. Messages
// never contain secrets.
package preflight

import (
	"context"
	"crypto/rand"
	"crypto/x509"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/tricknotes/koroku/internal/backup"
	"github.com/tricknotes/koroku/internal/config"
	"github.com/tricknotes/koroku/internal/domains"
	"github.com/tricknotes/koroku/internal/github"
	"github.com/tricknotes/koroku/internal/objstore"
)

// Status is the outcome of one check.
type Status int

const (
	// OK: nothing to do.
	OK Status = iota
	// Warn: koroku works, but something is missing or worth a look.
	Warn
	// Fail: a feature does not work until this is fixed.
	Fail
	// Skip: the check does not apply to this configuration.
	Skip
)

func (s Status) String() string {
	switch s {
	case OK:
		return "ok"
	case Warn:
		return "warn"
	case Fail:
		return "fail"
	default:
		return "skip"
	}
}

// Result is the outcome of one check.
type Result struct {
	Name    string
	Status  Status
	Message string
	// Hint says what to do; empty when Status is OK or Skip.
	Hint string
}

// GitHubApp is the part of the GitHub client the checks use.
type GitHubApp interface {
	App(ctx context.Context) (github.App, error)
	Installations(ctx context.Context) ([]github.Installation, error)
}

// GitHubWebhook reads the App's webhook settings. The client of Deps.GitHub
// implements it; a fake may not.
type GitHubWebhook interface {
	WebhookConfig(ctx context.Context) (github.WebhookConfig, error)
}

// GitHubUsers looks up public accounts.
type GitHubUsers interface {
	UserByID(ctx context.Context, id int64) (github.User, error)
}

// Backups lists the system snapshots of the restic repository.
type Backups interface {
	SystemSnapshots(ctx context.Context) ([]backup.Snapshot, error)
}

// Deps is what the checks read. Fields left nil make the matching checks
// report why they could not run.
type Deps struct {
	Config config.Config

	// Resolver answers DNS lookups. Nil means net.DefaultResolver.
	Resolver domains.Resolver
	// HostAddrs lists the addresses of this host's interfaces. Nil means
	// net.InterfaceAddrs.
	HostAddrs func() ([]net.Addr, error)
	// HTTP is used for the Caddy admin API and the console over HTTPS.
	HTTP *http.Client

	// GitHub is the App client, or nil with GitHubErr saying why it could not
	// be built (for example an unreadable private key).
	GitHub    GitHubApp
	GitHubErr error
	Users     GitHubUsers
	// AutoDeployApps are the apps that deploy on push (ARCHITECTURE 5.2);
	// AutoDeployErr says why they could not be read.
	AutoDeployApps []string
	AutoDeployErr  error
	// WorkflowApps are the apps among them that deploy only after a
	// workflow succeeded; WorkflowAppsErr says why they could not be read.
	WorkflowApps    []string
	WorkflowAppsErr error

	// Objects is the bucket of [storage], nil when it is not configured.
	Objects objstore.Store
	// Backups is the restic repository, nil when backups are not configured;
	// BackupsErr then says why.
	Backups    Backups
	BackupsErr error

	// Now is the clock; nil means time.Now.
	Now func() time.Time
}

// Run performs every check, in the order of docs/SETUP.md.
func Run(ctx context.Context, d Deps) []Result {
	if d.Resolver == nil {
		d.Resolver = net.DefaultResolver
	}
	if d.HostAddrs == nil {
		d.HostAddrs = net.InterfaceAddrs
	}
	if d.HTTP == nil {
		d.HTTP = &http.Client{Timeout: 15 * time.Second}
	}
	if d.Now == nil {
		d.Now = time.Now
	}
	var out []Result
	out = append(out, checkPlaceholders(d.Config))
	out = append(out, checkPublicIP(d))
	out = append(out, checkDNS(ctx, d)...)
	out = append(out, checkCaddy(ctx, d))
	out = append(out, checkHTTPS(ctx, d))
	out = append(out, checkMasterKey(d.Config))
	out = append(out, checkGitHub(ctx, d)...)
	out = append(out, checkStorage(ctx, d))
	out = append(out, checkBackups(ctx, d))
	return out
}

func ok(name, format string, args ...any) Result {
	return Result{Name: name, Status: OK, Message: fmt.Sprintf(format, args...)}
}

func skip(name, format string, args ...any) Result {
	return Result{Name: name, Status: Skip, Message: fmt.Sprintf(format, args...)}
}

func warn(name, hint, format string, args ...any) Result {
	return Result{Name: name, Status: Warn, Message: fmt.Sprintf(format, args...), Hint: hint}
}

func fail(name, hint, format string, args ...any) Result {
	return Result{Name: name, Status: Fail, Message: fmt.Sprintf(format, args...), Hint: hint}
}

// isPlaceholder matches the values of config.example.toml.
func isPlaceholder(v string) bool {
	return strings.Contains(v, "example.com") || strings.HasPrefix(v, "203.0.113.")
}

func checkPlaceholders(cfg config.Config) Result {
	const name = "config"
	var keys []string
	for key, v := range map[string]string{
		"base_domain":      cfg.BaseDomain,
		"console_host":     cfg.ConsoleHost,
		"public_ip":        cfg.PublicIP,
		"storage.endpoint": cfg.Storage.Endpoint,
		"caddy.acme_email": cfg.Caddy.ACMEEmail,
	} {
		if isPlaceholder(v) {
			keys = append(keys, key)
		}
	}
	if len(keys) > 0 {
		sort.Strings(keys)
		return fail(name, "replace the example values (docs/SETUP.md 7)",
			"example values left in %s", strings.Join(keys, ", "))
	}
	return ok(name, "no example values left")
}

func checkPublicIP(d Deps) Result {
	const name = "public_ip"
	ip := net.ParseIP(d.Config.PublicIP)
	if ip == nil {
		return fail(name, "set public_ip to the global address of this server (docs/SETUP.md 7)",
			"public_ip is not set: custom domains cannot be verified")
	}
	addrs, err := d.HostAddrs()
	if err != nil {
		return warn(name, "", "%s (the addresses of this host could not be read: %v)", ip, err)
	}
	for _, a := range addrs {
		if n, isNet := a.(*net.IPNet); isNet && n.IP.Equal(ip) {
			return ok(name, "%s is an address of this host", ip)
		}
	}
	return warn(name, "check that public_ip is the global address the provider assigned to this server (docs/SETUP.md 2)",
		"%s is not an address of any interface of this host (fine only behind NAT)", ip)
}

func checkDNS(ctx context.Context, d Deps) []Result {
	cfg := d.Config
	ip := net.ParseIP(cfg.PublicIP)
	if ip == nil {
		return []Result{skip("dns", "needs public_ip")}
	}
	v := domains.Verifier{Resolver: d.Resolver, PublicIP: ip}
	var out []Result
	if err := v.Verify(ctx, cfg.ConsoleHost); err != nil {
		out = append(out, fail("dns.console", "add an A record for the console host (docs/SETUP.md 3)", "%v", err))
	} else {
		out = append(out, ok("dns.console", "%s -> %s", cfg.ConsoleHost, ip))
	}
	// A name nobody registered resolves only through a wildcard record.
	probe := "koroku-check-" + randomHex(4) + "." + cfg.BaseDomain
	switch err := v.Verify(ctx, probe); {
	case err == nil:
		out = append(out, ok("dns.apps", "*.%s -> %s (wildcard record)", cfg.BaseDomain, ip))
	case errors.Is(err, domains.ErrNotFound):
		out = append(out, warn("dns.apps",
			"add *."+cfg.BaseDomain+", or an A record for every <slug>."+cfg.BaseDomain+" before deploying it (docs/SETUP.md 3)",
			"no wildcard record for *.%s", cfg.BaseDomain))
	default:
		out = append(out, fail("dns.apps", "point *."+cfg.BaseDomain+" at public_ip (docs/SETUP.md 3)",
			"*.%s: %v", cfg.BaseDomain, err))
	}
	return out
}

func checkCaddy(ctx context.Context, d Deps) Result {
	const name = "caddy"
	if !d.Config.Caddy.Enabled {
		return skip(name, "caddy.enabled is false")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimRight(d.Config.Caddy.AdminURL, "/")+"/config/", nil)
	if err != nil {
		return fail(name, "", "caddy.admin_url: %v", err)
	}
	resp, err := d.HTTP.Do(req)
	if err != nil {
		return fail(name, "start Caddy: systemctl status caddy (docs/OPERATIONS.md 8.3)", "admin API does not answer: %v", err)
	}
	drain(resp)
	if resp.StatusCode != http.StatusOK {
		return fail(name, "see journalctl -u caddy", "admin API answered %s", resp.Status)
	}
	return ok(name, "admin API answers on %s", d.Config.Caddy.AdminURL)
}

// stagingMarker is part of the Let's Encrypt staging directory URL.
const stagingMarker = "staging"

func checkHTTPS(ctx context.Context, d Deps) Result {
	const name = "https"
	cfg := d.Config
	if !cfg.Caddy.Enabled || cfg.Caddy.InternalCA {
		return skip(name, "certificates are not issued by ACME on this host")
	}
	if !strings.HasPrefix(cfg.ConsoleURL, "https://") {
		return skip(name, "console_url is not https")
	}
	staging := strings.Contains(cfg.Caddy.ACMECA, stagingMarker)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, cfg.ConsoleURL+"/api/system", nil)
	if err != nil {
		return fail(name, "", "%v", err)
	}
	resp, err := d.HTTP.Do(req)
	if err != nil {
		return httpsFailure(err, cfg.ConsoleURL, cfg.ConsoleHost, staging)
	}
	drain(resp)
	if staging {
		// Only reachable when the system trusts the staging root.
		return warn(name, "delete the acme_ca line for a real certificate (docs/SETUP.md 9)",
			"%s answers over HTTPS, but acme_ca is still the staging directory", cfg.ConsoleHost)
	}
	return ok(name, "%s answers over HTTPS with a trusted certificate", cfg.ConsoleHost)
}

// stagingIssuer is part of the issuer names of Let's Encrypt staging
// certificates ("(STAGING) Pretend Pear X1", "(STAGING) Let's Encrypt").
const stagingIssuer = "(STAGING)"

// httpsFailure explains a failed HTTPS request to the console. A
// certificate from an unknown authority is told apart by its issuer: a
// staging certificate while acme_ca still is the staging directory is
// expected, one served after switching away from it is a certificate Caddy
// still holds.
func httpsFailure(err error, consoleURL, host string, staging bool) Result {
	const name = "https"
	var unknownCA x509.UnknownAuthorityError
	if errors.As(err, &unknownCA) && unknownCA.Cert != nil {
		issuer := unknownCA.Cert.Issuer.CommonName
		if issuer == "" && len(unknownCA.Cert.Issuer.Organization) > 0 {
			issuer = unknownCA.Cert.Issuer.Organization[0]
		}
		isStaging := strings.Contains(issuer, stagingIssuer) ||
			slices.ContainsFunc(unknownCA.Cert.Issuer.Organization, func(o string) bool { return strings.Contains(o, stagingIssuer) })
		switch {
		case staging:
			return warn(name, "when everything works, delete the acme_ca line and restart koroku (docs/SETUP.md 9)",
				"%s serves a Let's Encrypt staging certificate (browsers will not trust it)", host)
		case isStaging:
			return fail(name,
				"acme_ca is no longer the staging directory, but Caddy keeps the certificate it already has: run sudo systemctl restart caddy (docs/OPERATIONS.md 12)",
				"%s still serves a Let's Encrypt staging certificate (issuer %q)", host, issuer)
		default:
			return fail(name, "see journalctl -u caddy (docs/OPERATIONS.md 12)",
				"%s serves a certificate from an unknown authority (issuer %q)", host, issuer)
		}
	}
	return fail(name,
		"Caddy obtains the certificate once DNS points here and 80/443 are open, also in the provider's firewall (docs/SETUP.md 2, 3); see journalctl -u caddy",
		"%s: %v", consoleURL, err)
}

func checkMasterKey(cfg config.Config) Result {
	const name = "master_key"
	info, err := os.Stat(cfg.MasterKeyFile)
	if errors.Is(err, os.ErrNotExist) {
		return warn(name, "start koroku once (systemctl restart koroku), then keep a copy outside the host until the first backup (docs/SETUP.md 6)",
			"%s does not exist yet; koroku creates it at its first start", cfg.MasterKeyFile)
	}
	if err != nil {
		return fail(name, "", "%v", err)
	}
	if info.Mode().Perm()&0o077 != 0 {
		return fail(name, "run: chmod 600 "+cfg.MasterKeyFile, "%s has mode %04o", cfg.MasterKeyFile, info.Mode().Perm())
	}
	return ok(name, "%s exists (mode 0600)", cfg.MasterKeyFile)
}

// wantedPermissions are the permissions the App needs (ARCHITECTURE 5).
var wantedPermissions = map[string]string{"contents": "read", "metadata": "read"}

// optionalPermissions are permissions the App may have; without them a
// feature is off. deployments: write lets koroku record its deployments on
// GitHub (ARCHITECTURE 5.1); actions: read lets the App subscribe to
// workflow runs, which apps that wait for a workflow deploy on (5.2).
var optionalPermissions = map[string]string{"deployments": "write", "actions": "read", "pull_requests": "read"}

func checkGitHub(ctx context.Context, d Deps) []Result {
	cfg := d.Config.GitHub
	if !cfg.Configured() {
		return []Result{fail("github.app", "register a GitHub App and fill in [github] (docs/SETUP.md 4)",
			"[github] is not configured: nobody can log in")}
	}
	if d.GitHub == nil {
		msg := "the GitHub client could not be built"
		if d.GitHubErr != nil {
			msg = d.GitHubErr.Error()
		}
		return []Result{fail("github.app", "check private_key_file (docs/SETUP.md 4)", "%s", msg)}
	}
	var out []Result
	app, err := d.GitHub.App(ctx)
	switch {
	case errors.Is(err, github.ErrUnauthorized):
		out = append(out, fail("github.app", "app_id and private_key_file must belong to the same App; generate a new key if unsure (docs/SETUP.md 4)",
			"GitHub rejected the app JWT"))
	case err != nil:
		out = append(out, fail("github.app", "", "%v", err))
	case app.ID != cfg.AppID:
		out = append(out, fail("github.app", "", "GitHub reports app ID %d, config has %d", app.ID, cfg.AppID))
	case app.ClientID != "" && app.ClientID != cfg.ClientID:
		out = append(out, fail("github.app", "copy the Client ID from the App's settings page (docs/SETUP.md 4)",
			"client_id does not match the App %q", app.Slug))
	default:
		out = append(out, permissionResult(app, d.Config.ConsoleURL))
	}

	if err == nil {
		out = append(out, installationResult(ctx, d.GitHub, app, cfg))
	}
	out = append(out, userResult(ctx, d.Users, cfg.AllowedUserID))
	out = append(out, webhookResult(ctx, d, app, err == nil))
	out = append(out, workflowResult(d, app, err == nil))
	return out
}

// workflowResult checks what apps that deploy after a workflow succeeded
// need (ARCHITECTURE 5.2): the Actions read permission, without which the
// App cannot subscribe to workflow runs, and that subscription.
func workflowResult(d Deps, app github.App, appKnown bool) Result {
	const name = "github.workflow"
	const hint = "docs/SETUP.md 4.6"
	if d.WorkflowAppsErr != nil {
		return warn(name, "", "could not read which apps wait for a workflow: %v", d.WorkflowAppsErr)
	}
	apps := d.WorkflowApps
	if len(apps) == 0 {
		return skip(name, "no app waits for a workflow before deploying")
	}
	if !appKnown {
		return skip(name, "the App could not be read to check its permissions and events")
	}
	if level := app.Permissions["actions"]; level != "read" && level != "write" {
		return warn(name, "set Repository permissions > Actions to Read-only, then accept the new permission on the installation ("+hint+")",
			"App %q lacks actions: read: %s will not deploy", app.Slug, strings.Join(apps, ", "))
	}
	if !slices.Contains(app.Events, "workflow_run") {
		return warn(name, "subscribe the App to the Workflow run event under Permissions & events > Subscribe to events ("+hint+")",
			"App %q does not subscribe to workflow_run events: %s will not deploy", app.Slug, strings.Join(apps, ", "))
	}
	return ok(name, "the App delivers workflow runs; %s deploy after their workflow succeeds", strings.Join(apps, ", "))
}

// webhookResult checks what automatic deployment on push needs
// (ARCHITECTURE 5.2): the webhook secret, the App's subscription to the
// push event and the webhook URL. Nothing is needed while no app uses it.
func webhookResult(ctx context.Context, d Deps, app github.App, appKnown bool) Result {
	const name = "github.webhook"
	const hint = "docs/SETUP.md 4"
	wantURL := d.Config.ConsoleURL + "/webhooks/github"
	if d.AutoDeployErr != nil {
		return warn(name, "", "could not read which apps deploy on push: %v", d.AutoDeployErr)
	}
	apps := d.AutoDeployApps
	secret := d.Config.GitHub.WebhookSecret != ""
	if len(apps) == 0 && !secret {
		return skip(name, "no app deploys on push")
	}
	if !secret {
		return warn(name, "set [github] webhook_secret to the App's webhook secret and restart koroku ("+hint+")",
			"%s deploy on push, but [github] webhook_secret is not set: every push is refused", strings.Join(apps, ", "))
	}
	if !appKnown {
		return skip(name, "webhook secret is set; the App could not be read to check its events")
	}
	if !slices.Contains(app.Events, "push") {
		if len(apps) == 0 {
			return ok(name, "webhook secret is set; no app deploys on push (the App does not subscribe to push events)")
		}
		return warn(name, "subscribe the App to the Push event under Permissions & events > Subscribe to events ("+hint+")",
			"App %q does not subscribe to push events: %s will not deploy on push", app.Slug, strings.Join(apps, ", "))
	}
	if hooks, ok := d.GitHub.(GitHubWebhook); ok {
		hook, err := hooks.WebhookConfig(ctx)
		switch {
		case err != nil:
			return warn(name, "", "could not read the App's webhook settings: %v", err)
		case hook.URL != wantURL:
			return warn(name, "set Webhook URL to "+wantURL+" and Active on ("+hint+")",
				"the App delivers its webhook to %q", hook.URL)
		case hook.ContentType != "json":
			return warn(name, "the App's webhook must send application/json ("+hint+")",
				"the App's webhook content type is %q", hook.ContentType)
		}
	}
	if len(apps) == 0 {
		return ok(name, "the App delivers push events to %s; no app deploys on push yet", wantURL)
	}
	return ok(name, "the App delivers push events to %s (keep the webhook Active); %s deploy on push", wantURL, strings.Join(apps, ", "))
}

func permissionResult(app github.App, consoleURL string) Result {
	const name = "github.app"
	var missing, extra []string
	for perm, level := range wantedPermissions {
		if app.Permissions[perm] != level {
			missing = append(missing, perm+": "+level)
		}
	}
	for perm, level := range app.Permissions {
		_, wanted := wantedPermissions[perm]
		if optional, ok := optionalPermissions[perm]; ok && level == optional {
			continue
		}
		if !wanted {
			extra = append(extra, perm+": "+level)
		}
	}
	sort.Strings(missing)
	sort.Strings(extra)
	callback := "callback URL must be " + consoleURL + "/auth/github/callback"
	if len(missing) > 0 {
		return fail(name, "set Repository permissions > Contents to Read-only (docs/SETUP.md 4)",
			"App %q lacks %s", app.Slug, strings.Join(missing, ", "))
	}
	if len(extra) > 0 {
		return warn(name, "koroku needs Contents and Metadata read-only, Deployments read and write, and (to wait for workflows) Actions read-only and (for review apps) Pull requests read-only only; remove the rest (docs/SETUP.md 4)",
			"App %q also has %s", app.Slug, strings.Join(extra, ", "))
	}
	if app.Permissions["deployments"] != optionalPermissions["deployments"] {
		return warn(name, "set Repository permissions > Deployments to Read and write, then accept the new permission on the installation (docs/SETUP.md 4)",
			"App %q lacks deployments: write; GitHub deployments will not be updated", app.Slug)
	}
	if app.Permissions["pull_requests"] != "read" && app.Permissions["pull_requests"] != "write" {
		return warn(name, "set Repository permissions > Pull requests to Read-only, then accept the new permission on the installation (docs/SETUP.md 4)",
			"App %q lacks pull_requests: read; review apps cannot be created", app.Slug)
	}
	return ok(name, "App %q (ID %d), Contents and Metadata read-only, Deployments read and write, Pull requests read-only; %s", app.Slug, app.ID, callback)
}

// installationResult lists the accounts the App is installed on with their
// IDs, and says what to write in config.toml for the accounts koroku
// ignores or matches by login only (ARCHITECTURE 5).
func installationResult(ctx context.Context, gh GitHubApp, app github.App, cfg config.GitHub) Result {
	const name = "github.install"
	list, err := gh.Installations(ctx)
	if err != nil {
		return fail(name, "", "%v", err)
	}
	if len(list) == 0 {
		hint := "install the App on the account that owns the repositories (docs/SETUP.md 4)"
		if app.HTMLURL != "" {
			hint = "install it: " + app.HTMLURL + "/installations/new (docs/SETUP.md 4)"
		}
		return warn(name, hint, "the App is not installed anywhere: private repositories cannot be listed or fetched")
	}
	// ids go on the installation_account_ids line that replaces the
	// logins: what is configured plus the accounts matched by login only.
	ids := slices.Clone(cfg.InstallationAccountIDs)
	var used, ignored, byLogin []string
	for _, in := range list {
		if !in.Allowed {
			ignored = append(ignored, fmt.Sprintf("%s (ID %d)", in.Account, in.AccountID))
			continue
		}
		used = append(used, fmt.Sprintf("%s (ID %d, %s repositories)", in.Account, in.AccountID, in.RepositorySelection))
		if cfg.InstallationAccess(in.AccountID, in.Account) == config.InstallationByLogin {
			byLogin = append(byLogin, in.Account)
			if !slices.Contains(ids, in.AccountID) {
				ids = append(ids, in.AccountID)
			}
		}
	}
	msg := "installed on " + strings.Join(used, ", ")
	var hints []string
	if len(ignored) > 0 {
		if len(used) == 0 {
			used = []string{"none"}
		}
		msg = fmt.Sprintf("installed on %s, which koroku ignores; used: %s", strings.Join(ignored, ", "), strings.Join(used, ", "))
		hints = append(hints, "add the ID of an account you manage to [github] installation_account_ids; uninstall the App from accounts you do not know")
	}
	// A login does not identify an account: a renamed account frees its
	// name, and whoever takes it and installs the App would be accepted.
	switch {
	case len(byLogin) > 0:
		msg += fmt.Sprintf("; %s matched by login only (installation_accounts), and a login given up in a rename can be taken by another account",
			strings.Join(byLogin, ", "))
		hints = append(hints, fmt.Sprintf("write %s in [github] and remove installation_accounts", accountIDsLine(ids)))
	case len(cfg.InstallationAccounts) > 0:
		msg += "; installation_accounts lists logins that no used installation needs, and a listed login can be taken by another account"
		hints = append(hints, "remove installation_accounts from [github]")
	}
	if len(hints) == 0 {
		return ok(name, "%s", msg)
	}
	return warn(name, strings.Join(hints, "; ")+" (docs/SETUP.md 4)", "%s", msg)
}

// accountIDsLine is the installation_account_ids line of config.toml for
// these account IDs.
func accountIDsLine(ids []int64) string {
	parts := make([]string, len(ids))
	for i, id := range ids {
		parts[i] = strconv.FormatInt(id, 10)
	}
	return "installation_account_ids = [" + strings.Join(parts, ", ") + "]"
}

func userResult(ctx context.Context, users GitHubUsers, id int64) Result {
	const name = "github.user"
	if users == nil {
		return skip(name, "no client to look the user up")
	}
	user, err := users.UserByID(ctx, id)
	if errors.Is(err, github.ErrNotFound) {
		return fail(name, "use the numeric id of https://api.github.com/users/<login> (docs/SETUP.md 4)",
			"no GitHub account has the ID %d", id)
	}
	if err != nil {
		return warn(name, "", "could not look up user %d: %v", id, err)
	}
	return ok(name, "only %s (ID %d) can log in", user.Login, user.ID)
}

// checkPrefix is where the storage check writes its probe object. It is
// outside logs/ and restic/, so lifecycle rules and restic never see it.
const checkPrefix = "koroku-check/"

func checkStorage(ctx context.Context, d Deps) Result {
	const name = "storage"
	if !d.Config.Storage.Configured() {
		return fail(name, "create a bucket and an access key and fill in [storage] (docs/SETUP.md 5)",
			"[storage] is not configured: no backups and no log archive")
	}
	if d.Objects == nil {
		return fail(name, "", "the storage client could not be built")
	}
	hint := "the access key must be allowed to read, write, list and delete objects in bucket " + d.Config.Storage.Bucket + " (docs/SETUP.md 5)"
	key := checkPrefix + randomHex(8)
	body := []byte("written by koroku check; safe to delete\n")
	if err := d.Objects.Put(ctx, key, body, "text/plain"); err != nil {
		return fail(name, hint, "write %s: %v", key, err)
	}
	// Clean up even when a later step fails.
	defer func() { _ = d.Objects.Delete(context.WithoutCancel(ctx), key) }()
	r, err := d.Objects.Get(ctx, key)
	if err != nil {
		return fail(name, hint, "read %s back: %v", key, err)
	}
	got, err := io.ReadAll(r)
	_ = r.Close()
	if err != nil || string(got) != string(body) {
		return fail(name, hint, "read %s back: content differs", key)
	}
	list, err := d.Objects.List(ctx, checkPrefix)
	if err != nil {
		return fail(name, hint, "list %s: %v", checkPrefix, err)
	}
	if !slices.ContainsFunc(list, func(o objstore.Object) bool { return o.Key == key }) {
		return fail(name, hint, "list %s: the object just written is missing", checkPrefix)
	}
	if err := d.Objects.Delete(ctx, key); err != nil {
		return fail(name, hint, "delete %s: %v", key, err)
	}
	return ok(name, "bucket %s: write, read, list and delete work", d.Config.Storage.Bucket)
}

func checkBackups(ctx context.Context, d Deps) Result {
	const name = "backup"
	if !d.Config.Storage.Configured() {
		return skip(name, "needs [storage]")
	}
	if d.Backups == nil {
		msg := "backups are disabled"
		if d.BackupsErr != nil {
			msg += ": " + d.BackupsErr.Error()
		}
		return fail(name, "put the restic password in backup.restic_password_file (mode 0600) (docs/SETUP.md 6)", "%s", msg)
	}
	snaps, err := d.Backups.SystemSnapshots(ctx)
	if err != nil {
		return fail(name, "a wrong restic password or a token without access to restic/ (docs/SETUP.md 5, 6)",
			"the restic repository cannot be read: %v", err)
	}
	if len(snaps) == 0 {
		return warn(name, "run a backup from the console (システム > バックアップ > 今すぐ実行), then run koroku check again",
			"no system snapshot yet (the first backup creates the repository)")
	}
	latest := snaps[0].Time
	age := d.Now().Sub(latest)
	if age > 26*time.Hour {
		return warn(name, "see the backup status in the console", "the latest system snapshot is from %s (%s ago)",
			latest.Format(time.RFC3339), age.Round(time.Hour))
	}
	return ok(name, "latest system snapshot %s (%s)", snaps[0].ID, latest.Format(time.RFC3339))
}

func drain(resp *http.Response) {
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<16))
	_ = resp.Body.Close()
}

func randomHex(n int) string {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}
